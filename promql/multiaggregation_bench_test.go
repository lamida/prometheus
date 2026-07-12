// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/util/teststorage"
)

// BenchmarkMultiAggregationCandidate measures exactly the cost
// docs/query-planner-phase3-design.md §4 asks about: for a query shape
// where several aggregations share one common-subexpression-eliminated
// input (agg1(x) + agg2(x) + ... op0 aggN(x)), how much of the total,
// post-CSE cost is attributable to each aggregation's own independent
// rangeEvalAgg call (grouping + accumulation over the already-shared,
// already-cheap-to-obtain input), versus how much CSE's existing Strategy A
// wiring has already eliminated (recomputing/re-selecting the shared input
// itself)?
//
// This does not benchmark a MultiAggregation implementation, because one
// does not exist in this codebase: the phase 3 design doc's conclusion,
// reached by tracing the actual eval/rangeEvalAgg call path rather than
// benchmarking, is that Mimir's specific MultiAggregation optimization
// (avoiding redundant per-series re-pulls from a shared *streaming*
// operator) has no direct analogue to build here, since Prometheus's
// evaluator is not pull-based and CSE's existing cache-hit memoization
// already makes a shared node's *n*th consumer's "read" O(1) (a map lookup
// plus a shallow slice clone), not proportional to series count. What this
// benchmark measures instead is the four points of comparison needed to
// judge whether *some* fused-aggregation mechanism — necessarily a novel,
// Prometheus-specific design, not a port of Mimir's, since there is no
// per-series pull cost here to fuse away — could plausibly be worth
// building:
//
//   - "single": one aggregation, one input. The baseline cost of producing
//     the input once plus one aggregation's own grouping/accumulation.
//   - "shared_no_cse": N aggregations over the textually identical input,
//     CSE disabled. Cost should scale as N times the input-production cost
//     plus N times one aggregation's own cost, since nothing dedupes the
//     repeated input production.
//   - "shared_cse": the same query, CSE enabled. Cost should scale as one
//     input-production cost plus N times one aggregation's own cost, if
//     Strategy A's memoization is doing its job. The gap between this and
//     "shared_no_cse" is CSE's already-realized win. What (if anything)
//     remains above "single"-scaled-by-N-aggregations-worth-of-just-the-
//     aggregation-cost is the ceiling on what a fused-aggregation mechanism
//     could still capture.
//   - "unshared_cse": N aggregations over N textually distinct (but
//     identically-shaped) inputs, CSE enabled. CSE cannot help here (no
//     duplicate subexpression exists), so this isolates "N times the full
//     cost" as a reference point independent of CSE.
//
// Comparing "shared_cse" against "single" scaled by N, and against
// "unshared_cse", is what tells us whether the N-1 redundant
// grouping/accumulation passes remaining after CSE are a meaningful
// fraction of total query cost, or noise.
func BenchmarkMultiAggregationCandidate(b *testing.B) {
	stor := teststorage.New(b)
	stor.DisableCompactions()

	const interval = 10000 // 10s.
	numIntervals := 8640   // one day.

	baseOpts := promql.EngineOpts{
		Logger:     nil,
		Reg:        nil,
		MaxSamples: 50000000,
		Timeout:    100 * time.Second,
		Parser:     parser.NewParser(parser.Options{}),
	}
	cseOpts := baseOpts
	cseOpts.EnableCommonSubexpressionElimination = true

	noCSEEngine := promqltest.NewTestEngineWithOpts(b, baseOpts)
	cseEngine := promqltest.NewTestEngineWithOpts(b, cseOpts)

	if err := setupRangeQueryTestData(stor, noCSEEngine, interval, numIntervals); err != nil {
		b.Fatal(err)
	}

	// aggOps is deliberately more than one distinct aggregation operator
	// (not e.g. sum(x)+sum(x)+sum(x)): a real MultiAggregation-shaped query
	// combines *different* aggregations sharing one input (e.g. sum and
	// avg for a ratio, or sum/min/max for a summary), and each
	// aggregation's own accumulator state (see promql/engine.go's
	// aggregation()) differs by operator, so this exercises distinct code
	// paths per aggregation rather than one path N times.
	aggOps := []string{"sum", "avg", "min", "max", "stddev", "count"}

	type cardinality struct {
		name   string
		metric string // base metric name; "_ten"/"_hundred" appended below.
	}
	cardinalities := []cardinality{
		{name: "card=ten", metric: "a"},
		{name: "card=hundred", metric: "a"},
	}

	for ci, card := range cardinalities {
		suffix := []string{"_ten", "_hundred"}[ci]
		metric := card.metric + suffix

		for _, n := range []int{2, 4, 6} {
			ops := aggOps[:n]

			buildExpr := func(inputs []string) string {
				parts := make([]string, n)
				for i, op := range ops {
					parts[i] = fmt.Sprintf("%s(%s)", op, inputs[i])
				}
				return strings.Join(parts, " + ")
			}

			sameInput := make([]string, n)
			for i := range sameInput {
				sameInput[i] = metric
			}
			sharedExpr := buildExpr(sameInput)

			distinctInputs := make([]string, n)
			for i := range distinctInputs {
				// b_<suffix> has the same shape/cardinality as a_<suffix>
				// (see setupRangeQueryTestData) but is a different metric,
				// so no two of these textually-distinct selectors are
				// CSE-eligible duplicates of each other.
				if i%2 == 0 {
					distinctInputs[i] = metric
				} else {
					distinctInputs[i] = "b" + suffix
				}
			}
			// Only two metrics of this cardinality exist in the fixture
			// (a_*, b_*), so for n > 2 some inputs necessarily repeat one
			// of those two. That's fine: repeats of a_<suffix> are still
			// CSE-eligible among themselves, so run this case only with
			// CSE disabled, isolating "no sharing exploited" as a pure
			// reference point regardless of why.
			unsharedExpr := buildExpr(distinctInputs)

			singleExpr := fmt.Sprintf("%s(%s)", ops[0], metric)

			runRangeQueryBench(b, fmt.Sprintf("%s,n=%d,single", card.name, n), noCSEEngine, stor, singleExpr, numIntervals)
			runRangeQueryBench(b, fmt.Sprintf("%s,n=%d,shared_no_cse", card.name, n), noCSEEngine, stor, sharedExpr, numIntervals)
			runRangeQueryBench(b, fmt.Sprintf("%s,n=%d,shared_cse", card.name, n), cseEngine, stor, sharedExpr, numIntervals)
			runRangeQueryBench(b, fmt.Sprintf("%s,n=%d,unshared_no_cse", card.name, n), noCSEEngine, stor, unsharedExpr, numIntervals)
		}
	}
}

func runRangeQueryBench(b *testing.B, name string, engine *promql.Engine, stor *teststorage.TestStorage, expr string, numIntervals int) {
	b.Run(name, func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			qry, err := engine.NewRangeQuery(
				ctx, stor, nil, expr,
				time.Unix(0, 0),
				time.Unix(int64(numIntervals*10), 0), time.Minute)
			if err != nil {
				b.Fatal(err)
			}
			res := qry.Exec(ctx)
			if res.Err != nil {
				b.Fatal(res.Err)
			}
			qry.Close()
		}
	})
}
