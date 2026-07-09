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

// This file tests EngineOpts.EnableCommonSubexpressionElimination: the
// wiring, in promql/engine.go, that consults a promql/plan query plan to
// evaluate a duplicated subexpression once instead of once per occurrence.
// See docs/query-planner-phase2-design.md §4-§5.
//
// The most important test here is
// TestCommonSubexpressionElimination_BuiltinCorpus: it runs the ENTIRE
// promqltest builtin acceptance corpus (promql/promqltest/testdata/*.test —
// aggregators, operators, functions, histograms, subqueries, the @
// modifier, and more) through an engine with the feature enabled. Every
// eval command in that corpus asserts a specific expected result, so this
// is a correctness check against known-good values, not merely "the same
// answer as some other possibly-also-wrong engine".
//
// TestCommonSubexpressionElimination_Differential is the complementary,
// narrower check: a hand-picked set of queries specifically constructed to
// trigger sharing (including cases this package's materialization
// deliberately excludes: subquery- and step-invariant-nested duplicates),
// each run against a baseline (CSE disabled) and a CSE-enabled engine
// sharing the same storage, asserting byte-identical results.

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

// cseTestLoadData is shared fixture data for every hand-picked test in this
// file: two counters across a few label combinations, over a range wide
// enough for rate() and a short subquery. The full builtin corpus (run by
// TestCommonSubexpressionElimination_BuiltinCorpus) already covers
// histograms, staleness, and many other shapes; this fixture only needs to
// support the specific duplicated-subexpression shapes the differential
// cases below exercise.
const cseTestLoadData = `
load 30s
	http_requests_total{job="api", instance="1"} 0 10 20 30 40 50
	http_requests_total{job="api", instance="2"} 0 5 10 15 20 25
	http_requests_total{job="web", instance="1"} 100 110 120 130 140 150
	errors_total{job="api", instance="1"} 0 1 2 3 4 5
`

// newCSETestEngine returns a promql.Engine with
// EnableCommonSubexpressionElimination set to enableCSE and every other
// option held fixed across both variants a test compares, so any
// difference in results is attributable only to the flag under test.
func newCSETestEngine(t *testing.T, enableCSE bool, maxSamples int) *promql.Engine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:                           maxSamples,
		Timeout:                              100 * time.Second,
		NoStepSubqueryIntervalFn:             func(int64) int64 { return (30 * time.Second).Milliseconds() },
		EnableAtModifier:                     true,
		EnableNegativeOffset:                 true,
		EnableDelayedNameRemoval:             true,
		UseStartTimestamps:                   true,
		Parser:                               parser.NewParser(promqltest.TestParserOpts),
		EnableCommonSubexpressionElimination: enableCSE,
	})
}

// TestCommonSubexpressionElimination_BuiltinCorpus runs the full
// promqltest builtin acceptance corpus against a CSE-enabled engine. Every
// query in that corpus is checked against a fixed expected result baked
// into the corresponding testdata/*.test file, so this is the strongest
// evidence in this file that enabling the feature does not change any
// query's answer: instant and range queries, aggregations, binary
// operators, every builtin function, histograms, subqueries, and the @
// modifier are all covered, including many queries this file's other,
// hand-picked cases do not think to try.
func TestCommonSubexpressionElimination_BuiltinCorpus(t *testing.T) {
	engine := newCSETestEngine(t, true, 50000000)
	promqltest.RunBuiltinTests(t, engine)
}

// cseDifferentialCase is one query to run against both a baseline and a
// CSE-enabled engine, sharing the same storage and time range.
type cseDifferentialCase struct {
	name string
	expr string
	// instant, if true, runs an instant query at instantTime; otherwise a
	// range query over [rangeStart, rangeEnd] at rangeStep.
	instant bool
}

// cseDifferentialCases is a deliberately adversarial set of queries: every
// one contains a subexpression this package's plan.CommonSubexpressionElimination
// can detect as duplicated, chosen to exercise every sharing shape this
// step's design discusses.
var cseDifferentialCases = []cseDifferentialCase{
	// Self-duplicate at the leaf level: BinaryExprNode's two operands
	// materialize onto the literal same *parser.VectorSelector. This is
	// the scenario that specifically requires eval's cache-hit path to
	// return a fresh clone of the cached Matrix's outer []Series slice
	// (see cloneSharedValue's doc): naively returning the exact same
	// Matrix value to both of rangeEval's exprs slots would let
	// gatherVector's in-place per-step cursor advancement on one slot
	// corrupt the other.
	{name: "self_dup_vector_selector_instant", expr: `http_requests_total{job="api"} + http_requests_total{job="api"}`, instant: true},
	{name: "self_dup_vector_selector_range", expr: `http_requests_total{job="api"} + http_requests_total{job="api"}`, instant: false},

	// Duplicate through a Call wrapping a MatrixSelector: the whole
	// rate(...) chain (CallNode -> MatrixSelectorNode -> VectorSelectorNode)
	// merges into one shared subtree at the plan level.
	{name: "self_dup_rate_instant", expr: `rate(http_requests_total{job="api"}[1m]) / rate(http_requests_total{job="api"}[1m])`, instant: true},
	{name: "self_dup_rate_range", expr: `rate(http_requests_total{job="api"}[1m]) / rate(http_requests_total{job="api"}[1m])`, instant: false},

	// Duplicate nested inside two different AggregateExprs: neither
	// aggregation itself merges (sum vs sum used twice, same shape,
	// actually would merge too — use two different operations to keep
	// the aggregation nodes themselves distinct) but their shared
	// rate(...) argument does.
	{name: "dup_under_different_aggregations", expr: `sum(rate(http_requests_total{job="api"}[1m])) + max(rate(http_requests_total{job="api"}[1m]))`, instant: false},

	// Shared leaf under otherwise-distinct parents (abs/ceil): the
	// VectorSelectorNode merges even though its two parent CallNodes do
	// not.
	{name: "shared_leaf_different_parents", expr: `abs(http_requests_total{job="api"}) + ceil(http_requests_total{job="api"})`, instant: false},

	// "or" always wraps its BinaryExprNode in a DeduplicateAndMergeNode
	// (see promql/plan/dedup.go), which must be stripped before CSE runs
	// and must not interfere with sharing the operands underneath it.
	{name: "dup_under_or", expr: `http_requests_total{job="api"} or http_requests_total{job="api"}`, instant: false},

	// A Call with more than one duplicated argument (the vector argument
	// AND the scalar arguments all repeat).
	{name: "dup_call_args", expr: `clamp(http_requests_total{job="api"}, 0, 100) + clamp(http_requests_total{job="api"}, 0, 100)`, instant: false},

	// Exclusion boundary: the same rate(...) subexpression appears once
	// at top level and once inside a subquery body. plan.MaterializeSharing
	// must never alias these together (see occurrenceRecord.ineligibleForSharing
	// in promql/plan/materialize.go), but the query's RESULT must still be
	// correct regardless — this asserts that, not just "doesn't crash".
	{name: "exclusion_subquery_boundary", expr: `rate(http_requests_total{job="api"}[1m]) + sum_over_time(rate(http_requests_total{job="api"}[1m])[2m:30s])`, instant: false},

	// Exclusion boundary: the same rate(...) subexpression appears once
	// unpinned and once under an @ modifier (step-invariant). Also must
	// never be aliased together, and must still produce a correct result.
	{name: "exclusion_step_invariant_boundary", expr: `rate(http_requests_total{job="api"}[1m]) + rate(http_requests_total{job="api"}[1m] @ 60)`, instant: false},

	// Both exclusions on the very same query, to make sure they compose:
	// a top-level duplicate (materialized) alongside an excluded
	// subquery-nested copy of the SAME subexpression.
	{name: "exclusion_and_sharing_combined", expr: `(rate(http_requests_total{job="api"}[1m]) + rate(http_requests_total{job="api"}[1m])) + sum_over_time(rate(http_requests_total{job="api"}[1m])[2m:30s])`, instant: false},
}

// TestCommonSubexpressionElimination_Differential runs every case in
// cseDifferentialCases against a baseline (CSE disabled) and a
// CSE-enabled engine sharing the same storage and time range, and asserts
// the two engines return byte-identical results: same value, same
// warnings, same error-or-not. See this file's package doc for why this is
// a secondary, narrower check alongside
// TestCommonSubexpressionElimination_BuiltinCorpus.
func TestCommonSubexpressionElimination_Differential(t *testing.T) {
	storage := promqltest.LoadedStorage(t, cseTestLoadData)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	baseline := newCSETestEngine(t, false, 50000)
	withCSE := newCSETestEngine(t, true, 50000)

	const instantTime = 90 * time.Second

	for _, c := range cseDifferentialCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			var (
				baselineQuery, cseQuery promql.Query
				err                     error
			)
			if c.instant {
				at := time.Unix(0, 0).Add(instantTime)
				baselineQuery, err = baseline.NewInstantQuery(ctx, storage, nil, c.expr, at)
				require.NoError(t, err)
				cseQuery, err = withCSE.NewInstantQuery(ctx, storage, nil, c.expr, at)
				require.NoError(t, err)
			} else {
				start := time.Unix(0, 0)
				end := start.Add(150 * time.Second)
				step := 30 * time.Second
				baselineQuery, err = baseline.NewRangeQuery(ctx, storage, nil, c.expr, start, end, step)
				require.NoError(t, err)
				cseQuery, err = withCSE.NewRangeQuery(ctx, storage, nil, c.expr, start, end, step)
				require.NoError(t, err)
			}
			defer baselineQuery.Close()
			defer cseQuery.Close()

			baselineRes := baselineQuery.Exec(ctx)
			cseRes := cseQuery.Exec(ctx)

			if baselineRes.Err != nil {
				require.EqualError(t, cseRes.Err, baselineRes.Err.Error(), "expected the same error with and without CSE")
				return
			}
			require.NoError(t, cseRes.Err)
			testutil.RequireEqual(t, baselineRes.Warnings, cseRes.Warnings, "warnings differ between baseline and CSE for %q", c.expr)
			testutil.RequireEqual(t, baselineRes.Value, cseRes.Value, "result differs between baseline and CSE for %q", c.expr)
		})
	}
}

// TestCommonSubexpressionElimination_SampleLimitAccounting is the required
// regression test from docs/query-planner-phase2-design.md §5: a shared
// subexpression's samples must be counted once, not once per occurrence,
// against EngineOpts.MaxSamples. It runs the exact same query at two
// MaxSamples values one apart: the value that is exactly enough if the
// shared subexpression's samples are counted once must succeed, and one
// less must fail with the usual too-many-samples error — proving the CSE
// path is not double-counting a cache hit's samples via ev.currentSamples.
//
// Rather than hand-deriving the exact expected sample count (fragile: the
// evaluator's currentSamples bookkeeping mixes together the cost of
// actually computing a shared subexpression's value — which CSE really
// does deduplicate — with the cost of each individual consumer scanning
// its own copy of the result via gatherVector/aggregation — which happens
// once per consumer regardless of sharing, exactly as it would for two
// textually-identical but unshared subexpressions), this test measures the
// CSE-enabled query's actual peak sample count empirically via
// Query.Stats(), then uses that measured value as the exact boundary.
func TestCommonSubexpressionElimination_SampleLimitAccounting(t *testing.T) {
	storage := promqltest.LoadedStorage(t, cseTestLoadData)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	// A range-vector function over a window wide enough to read more than
	// one raw sample per series makes the actual storage-fetch cost (what
	// CSE deduplicates: the whole rate(...) CallNode is evaluated once, not
	// once per occurrence) the dominant contributor to the sample count,
	// unlike a plain instant vector selector (where gatherVector's fixed
	// one-sample-per-consumer-per-step bookkeeping would dominate and mask
	// any de-duplication).
	const expr = `rate(http_requests_total{job="api"}[2m]) + rate(http_requests_total{job="api"}[2m])`
	at := time.Unix(0, 0).Add(90 * time.Second)
	ctx := context.Background()

	measurePeakSamples := func(t *testing.T, enableCSE bool) int {
		t.Helper()
		engine := newCSETestEngine(t, enableCSE, 1<<30)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.NoError(t, res.Err)
		return qry.Stats().Samples.PeakSamples
	}

	baselinePeak := measurePeakSamples(t, false)
	csePeak := measurePeakSamples(t, true)
	require.Less(t, csePeak, baselinePeak, "expected enabling CSE to lower this query's peak sample count by de-duplicating the shared rate(...) computation")

	t.Run("exactly_measured_peak_succeeds", func(t *testing.T) {
		engine := newCSETestEngine(t, true, csePeak)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.NoError(t, res.Err, "expected the query to succeed when MaxSamples exactly matches the measured, de-duplicated peak sample count")
	})

	t.Run("one_fewer_than_measured_peak_fails", func(t *testing.T) {
		engine := newCSETestEngine(t, true, csePeak-1)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.Error(t, res.Err, "expected the query to fail when MaxSamples is one less than the measured, de-duplicated peak sample count")
		require.Contains(t, res.Err.Error(), "too many samples")
	})

	// Sanity check: the SAME MaxSamples budget that exactly suffices with
	// CSE enabled must NOT suffice with CSE disabled, confirming the
	// subtests above are actually exercising de-duplication and not some
	// unrelated accounting slack.
	t.Run("baseline_without_cse_needs_more_samples", func(t *testing.T) {
		engine := newCSETestEngine(t, false, csePeak)
		qry, err := engine.NewInstantQuery(ctx, storage, nil, expr, at)
		require.NoError(t, err)
		defer qry.Close()
		res := qry.Exec(ctx)
		require.Error(t, res.Err, "expected the unshared baseline to need more samples than the CSE-enabled peak for this query")
		require.Contains(t, res.Err.Error(), "too many samples")
	})
}
