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
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/util/teststorage"
)

// BenchmarkSubsetSelectorElimination is the real-engine counterpart to
// BenchmarkSubsetSelectorCandidate (subsetselector_bench_test.go): that
// benchmark predates EngineOpts.EnableSubsetSelectorElimination's
// engine wiring and, per its own doc, operates directly on a
// storage.Querier as a "hand-rolled prototype" because there was no
// plan-integrated node to benchmark yet. Now that SSE is wired into
// evalUncached (see promql/engine.go's *parser.VectorSelector case and
// evalSubsetSelector), this benchmarks actual query execution through a
// real *promql.Engine, to check whether the ~0.12-0.21 crossover ratio the
// prototype found (docs/query-planner-phase4-design.md §6-§7) still holds
// once real overhead — map lookups for subsetSource/sharedNodeRefcount,
// cloneSharedValue, refcount bookkeeping in shouldReleaseSharedNode — is
// included, or whether that overhead shifts the crossover.
//
// The query shape is `sum(A) + sum(B)`, where B's matchers are A's plus one
// residual matcher (the subsumption relation SSE detects): an aggregation
// on each side avoids the result depending on output series order, and
// keeps the comparison focused on selector-level cost rather than on
// arithmetic binary-op matching semantics.
//
//   - "sse_disabled": both A and B run their own independent selection
//     from storage (today's default behavior).
//   - "sse_enabled": A is selected from storage once; B's result is
//     derived by filtering A's already-materialized Matrix in memory
//     instead of running its own storage.Querier.Select.
//
// The axes are the same as the prototype benchmark's: A's absolute
// cardinality (numSeries) and B's cardinality as a fraction of A's (ratio).
// The fixture here is purpose-built for this benchmark (larger than
// engine_sse_test.go's sseTestLoadData, which is sized for correctness
// tests, not performance measurement) and reused across every ratio via a
// "half"/"decile"/"percentile" label every series carries, exactly as the
// prototype benchmark does.
func BenchmarkSubsetSelectorElimination(b *testing.B) {
	const numPoints = 120 // 2 hours at 1-minute resolution.
	const interval = int64(60_000)

	ratios := []struct {
		name  string
		label string
		ratio float64
	}{
		{"ratio=0.50", "half", 0.5},
		{"ratio=0.10", "decile", 0.1},
		{"ratio=0.01", "percentile", 0.01},
	}

	baseOpts := promql.EngineOpts{
		MaxSamples: 50000000,
		Timeout:    100 * time.Second,
		Parser:     parser.NewParser(parser.Options{}),
	}
	sseOpts := baseOpts
	sseOpts.EnableSubsetSelectorElimination = true

	for _, numSeries := range []int{100, 1000, 10000} {
		stor := teststorage.New(b)
		stor.DisableCompactions()
		if err := setupSSEEngineBenchData(stor, numSeries, numPoints, interval); err != nil {
			b.Fatal(err)
		}

		noSSEEngine := promqltest.NewTestEngineWithOpts(b, baseOpts)
		sseEngine := promqltest.NewTestEngineWithOpts(b, sseOpts)

		for _, r := range ratios {
			if r.ratio*float64(numSeries) < 1 {
				continue // Not enough series at this cardinality to realize the ratio.
			}

			expr := fmt.Sprintf(
				`sum(sse_bench_test{job="x"}) + sum(sse_bench_test{job="x",%s="0"})`,
				r.label,
			)

			runSSEInstantQueryBench(b, fmt.Sprintf("series=%d,%s,sse_disabled", numSeries, r.name), noSSEEngine, stor, expr, numPoints, interval)
			runSSEInstantQueryBench(b, fmt.Sprintf("series=%d,%s,sse_enabled", numSeries, r.name), sseEngine, stor, expr, numPoints, interval)
		}
	}
}

func setupSSEEngineBenchData(stor *teststorage.TestStorage, numSeries, numPoints int, interval int64) error {
	ctx := context.Background()
	app := stor.Appender(ctx)
	for i := range numSeries {
		lbls := labels.FromStrings(
			"__name__", "sse_bench_test",
			"job", "x",
			"id", strconv.Itoa(i),
			"half", strconv.Itoa(i%2),
			"decile", strconv.Itoa(i%10),
			"percentile", strconv.Itoa(i%100),
		)
		for p := range numPoints {
			if _, err := app.Append(0, lbls, int64(p)*interval, float64(p)); err != nil {
				return err
			}
		}
	}
	return app.Commit()
}

// runSSEInstantQueryBench runs expr as an instant query at the fixture's
// last timestamp, so both operands' range selectors read every sample the
// fixture wrote (maximizing chunk-decode cost relative to selection
// overhead, mirroring the prototype benchmark's full-decode "unshared" and
// "shared_prototype" cases).
func runSSEInstantQueryBench(b *testing.B, name string, engine *promql.Engine, stor *teststorage.TestStorage, expr string, numPoints int, interval int64) {
	b.Run(name, func(b *testing.B) {
		ctx := context.Background()
		ts := time.UnixMilli((int64(numPoints) - 1) * interval)
		b.ReportAllocs()
		for b.Loop() {
			qry, err := engine.NewInstantQuery(ctx, stor, nil, expr, ts)
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
