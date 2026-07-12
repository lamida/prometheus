// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql_test

// This file tests EngineOpts.EnableSubsetSelectorElimination: the wiring,
// in promql/engine.go, that derives one selector's result from another
// already-materialized, wider selector's result by filtering in memory,
// instead of issuing its own storage.Querier.Select call. See
// docs/query-planner-phase4-design.md.
//
// Mirrors engine_cse_test.go's structure: a builtin-corpus run for broad
// correctness coverage, plus a hand-picked differential set targeting the
// specific sharing shapes this pass introduces (including the ones its own
// exclusions must still evaluate correctly: subquery/step-invariant
// boundaries, and selectors nested inside a MatrixSelector).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/util/testutil"
)

// sseTestLoadData is shared fixture data for this file: a metric with
// several overlapping matcher-subset relationships available (job, and a
// job+env pair, and a job+env+region triple), so a single load block
// supports every differential case below.
const sseTestLoadData = `
load 30s
	up{job="api", env="prod", region="us"} 1 1 1 1 1 1
	up{job="api", env="prod", region="eu"} 1 1 1 1 1 1
	up{job="api", env="staging", region="us"} 1 1 1 1 1 1
	up{job="web", env="prod", region="us"} 1 1 1 1 1 1
	http_requests_total{job="api", env="prod"} 0 10 20 30 40 50
	http_requests_total{job="api", env="staging"} 0 1 2 3 4 5
`

// newSSETestEngine returns a promql.Engine with
// EnableSubsetSelectorElimination set to enableSSE and every other option
// held fixed across both variants a test compares, so any difference in
// results is attributable only to the flag under test.
func newSSETestEngine(t *testing.T, enableSSE bool, maxSamples int) *promql.Engine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:                      maxSamples,
		Timeout:                         100 * time.Second,
		NoStepSubqueryIntervalFn:        func(int64) int64 { return (30 * time.Second).Milliseconds() },
		EnableAtModifier:                true,
		EnableNegativeOffset:            true,
		EnableDelayedNameRemoval:        true,
		UseStartTimestamps:              true,
		Parser:                          parser.NewParser(promqltest.TestParserOpts),
		EnableSubsetSelectorElimination: enableSSE,
	})
}

// TestSubsetSelectorElimination_BuiltinCorpus runs the full promqltest
// builtin acceptance corpus against an SSE-enabled engine. Every query in
// that corpus is checked against a fixed expected result baked into the
// corresponding testdata/*.test file, so this is the strongest evidence
// here that enabling the feature does not change any query's answer.
func TestSubsetSelectorElimination_BuiltinCorpus(t *testing.T) {
	engine := newSSETestEngine(t, true, 50000000)
	promqltest.RunBuiltinTests(t, engine)
}

// sseDifferentialCase is one query to run against both a baseline and an
// SSE-enabled engine, sharing the same storage and time range.
type sseDifferentialCase struct {
	name    string
	expr    string
	instant bool
}

// sseDifferentialCases is a deliberately adversarial set of queries: every
// one contains a matcher-subset relationship this package's
// plan.SubsetSelectorElimination can detect, chosen to exercise every
// sharing and exclusion shape docs/query-planner-phase4-design.md discusses.
var sseDifferentialCases = []sseDifferentialCase{
	// Basic subsumption at the leaf level, on both sides of a binary
	// operator: up{job="api"}'s result must be filtered, not re-selected,
	// to produce up{job="api",env="prod"}'s.
	{name: "basic_subsumption_instant", expr: `up{job="api"} + up{job="api",env="prod"}`, instant: true},
	{name: "basic_subsumption_range", expr: `up{job="api"} + up{job="api",env="prod"}`, instant: false},

	// A chain: up{job="api",env="prod"} derives from up{job="api"}, and
	// up{job="api",env="prod",region="us"} derives from
	// up{job="api",env="prod"} in turn (SubsetSelectorElimination picks the
	// cheapest, not necessarily the most specific, but a three-way query
	// like this still exercises more than one relation at once).
	{name: "three_way_chain", expr: `up{job="api"} + up{job="api",env="prod"} + up{job="api",env="prod",region="us"}`, instant: false},

	// Incomparable supersets: two selectors, neither a superset of the
	// other, both subsume a third. Only one relation should form, and the
	// result must still be correct regardless of which candidate SSE picks.
	{name: "incomparable_supersets", expr: `up{job="api"} + up{env="prod"} + up{job="api",env="prod"}`, instant: false},

	// Under aggregation and a range-vector function: the subsumed selector
	// sits inside rate(), not as a bare instant vector selector.
	{name: "subsumption_under_rate", expr: `sum(rate(http_requests_total{job="api"}[1m])) + sum(rate(http_requests_total{job="api",env="prod"}[1m]))`, instant: false},

	// Exclusion: the narrower selector's only occurrence sits inside a
	// subquery body, the wider one outside. plan.MaterializeSubsetSharing
	// must never relate these (see occurrenceRecord.ineligibleForSharing),
	// but the query's RESULT must still be correct regardless.
	{name: "exclusion_subquery_boundary", expr: `up{job="api"} + sum_over_time(up{job="api",env="prod"}[2m:30s])`, instant: false},

	// Exclusion: the narrower selector carries its own @ modifier
	// (step-invariant). Must not be related, and must still be correct.
	{name: "exclusion_step_invariant_boundary", expr: `up{job="api"} + up{job="api",env="prod"} @ 60`, instant: false},

	// Exclusion: a selector that would otherwise subsume another, but only
	// appears nested inside a MatrixSelector ([1m]) — see
	// SubsetSelectorElimination's MatrixSelectorNode exclusion doc. The
	// bare instant selector below must still evaluate normally (via its own
	// Select), not attempt to derive from the matrix-nested one. Uses
	// http_requests_total (a counter), not up, so rate() does not also
	// emit an unrelated "might not be a counter" annotation that would
	// otherwise be present regardless of SSE.
	{name: "exclusion_matrix_nested_source", expr: `rate(http_requests_total{job="api"}[1m]) + http_requests_total{job="api",env="prod"}`, instant: false},

	// No relation at all: disjoint matcher sets. Exercises the "found == 0,
	// SSE contributes nothing" path alongside the other cases in the same
	// test run.
	{name: "no_relation_disjoint_matchers", expr: `up{job="api"} + up{job="web"}`, instant: false},

	// A subsumed selector used standalone (not inside a binary expr): its
	// vector-typed result must still convert to Vector/Matrix correctly at
	// the top level for both instant and range queries.
	{name: "standalone_subsumed_selector_instant", expr: `up{job="api",env="prod"}`, instant: true},
	{name: "standalone_subsumed_selector_range", expr: `up{job="api",env="prod"}`, instant: false},
}

// TestSubsetSelectorElimination_Differential runs every case in
// sseDifferentialCases against a baseline (SSE disabled) and an
// SSE-enabled engine sharing the same storage and time range, and asserts
// the two engines return byte-identical results.
func TestSubsetSelectorElimination_Differential(t *testing.T) {
	storage := promqltest.LoadedStorage(t, sseTestLoadData)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	baseline := newSSETestEngine(t, false, 50000)
	withSSE := newSSETestEngine(t, true, 50000)

	const instantTime = 90 * time.Second

	for _, c := range sseDifferentialCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			var (
				baselineQuery, sseQuery promql.Query
				err                     error
			)
			if c.instant {
				at := time.Unix(0, 0).Add(instantTime)
				baselineQuery, err = baseline.NewInstantQuery(ctx, storage, nil, c.expr, at)
				require.NoError(t, err)
				sseQuery, err = withSSE.NewInstantQuery(ctx, storage, nil, c.expr, at)
				require.NoError(t, err)
			} else {
				start := time.Unix(0, 0)
				end := start.Add(150 * time.Second)
				step := 30 * time.Second
				baselineQuery, err = baseline.NewRangeQuery(ctx, storage, nil, c.expr, start, end, step)
				require.NoError(t, err)
				sseQuery, err = withSSE.NewRangeQuery(ctx, storage, nil, c.expr, start, end, step)
				require.NoError(t, err)
			}
			defer baselineQuery.Close()
			defer sseQuery.Close()

			baselineRes := baselineQuery.Exec(ctx)
			sseRes := sseQuery.Exec(ctx)

			if baselineRes.Err != nil {
				require.EqualError(t, sseRes.Err, baselineRes.Err.Error(), "expected the same error with and without SSE")
				return
			}
			require.NoError(t, sseRes.Err)
			testutil.RequireEqual(t, baselineRes.Warnings, sseRes.Warnings, "warnings differ between baseline and SSE for %q", c.expr)
			testutil.RequireEqual(t, baselineRes.Value, sseRes.Value, "result differs between baseline and SSE for %q", c.expr)
		})
	}
}

// TestSubsetSelectorElimination_CombinedWithCSE runs a query shape that
// exercises both features enabled together: a CSE-shared subexpression
// alongside an SSE-derived one, in the same query, asserting the combined
// result still matches a plain baseline.
func TestSubsetSelectorElimination_CombinedWithCSE(t *testing.T) {
	storage := promqltest.LoadedStorage(t, sseTestLoadData)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	const expr = `(up{job="api"} + up{job="api"}) + up{job="api",env="prod"}`

	baseline := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: 50000,
		Timeout:    100 * time.Second,
		Parser:     parser.NewParser(promqltest.TestParserOpts),
	})
	combined := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:                           50000,
		Timeout:                              100 * time.Second,
		Parser:                               parser.NewParser(promqltest.TestParserOpts),
		EnableCommonSubexpressionElimination: true,
		EnableSubsetSelectorElimination:      true,
	})

	ctx := context.Background()
	start := time.Unix(0, 0)
	end := start.Add(150 * time.Second)
	step := 30 * time.Second

	baselineQuery, err := baseline.NewRangeQuery(ctx, storage, nil, expr, start, end, step)
	require.NoError(t, err)
	defer baselineQuery.Close()
	combinedQuery, err := combined.NewRangeQuery(ctx, storage, nil, expr, start, end, step)
	require.NoError(t, err)
	defer combinedQuery.Close()

	baselineRes := baselineQuery.Exec(ctx)
	combinedRes := combinedQuery.Exec(ctx)

	require.NoError(t, baselineRes.Err)
	require.NoError(t, combinedRes.Err)
	testutil.RequireEqual(t, baselineRes.Value, combinedRes.Value)
}

// TestSubsetSelectorElimination_SampleLimitAccounting checks that a
// subsumed selector's samples are not double-counted: since its result is
// derived by filtering its source's already-materialized Matrix rather
// than re-selecting from storage, its own contribution to
// ev.currentSamples should be nothing beyond what evaluating the source
// once already charged (this mirrors
// TestCommonSubexpressionElimination_SampleLimitAccounting's approach of
// measuring the actual peak empirically rather than hand-deriving it).
//
// This deliberately uses bare vector selectors, not a range-vector function
// like rate(): SubsetSelectorElimination excludes any selector reachable
// only as a MatrixSelectorNode's child (see its doc), so rate()'s own
// sample accounting is untouched by this feature — only the plain
// evalSeries path evalUncached's *parser.VectorSelector case takes is.
func TestSubsetSelectorElimination_SampleLimitAccounting(t *testing.T) {
	storage := promqltest.LoadedStorage(t, sseTestLoadData)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	const expr = `up{job="api"} + up{job="api",env="prod"}`
	at := time.Unix(0, 0).Add(90 * time.Second)
	ctx := context.Background()

	measurePeakSamples := func(t *testing.T, enableSSE bool) int {
		t.Helper()
		engine := newSSETestEngine(t, enableSSE, 1<<30)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.NoError(t, res.Err)
		return qry.Stats().Samples.PeakSamples
	}

	baselinePeak := measurePeakSamples(t, false)
	ssePeak := measurePeakSamples(t, true)
	require.Less(t, ssePeak, baselinePeak, "expected enabling SSE to lower this query's peak sample count by deriving the narrower rate(...) input from the wider one instead of re-selecting it")

	t.Run("exactly_measured_peak_succeeds", func(t *testing.T) {
		engine := newSSETestEngine(t, true, ssePeak)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.NoError(t, res.Err, "expected the query to succeed when MaxSamples exactly matches the measured, de-duplicated peak sample count")
	})
}
