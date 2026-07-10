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

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
)

// BenchmarkSubsetSelectorCandidate implements the benchmark
// docs/query-planner-phase4-design.md §6 asks for, in place of building
// FilteredSelectorNode blind: for a selector pair A/B where B's matcher set
// is A's plus one residual matcher (the "Subset Selector Elimination"
// relation §1 defines), is it cheaper to fetch A and B independently
// (today's behavior) or to fetch A once and derive B by filtering A's
// already-materialized series set in memory (the sharing this document
// asks whether to build)?
//
// Two scenarios, both operating directly on a storage.Querier (there is no
// plan-integrated FilteredSelectorNode to benchmark; this is the
// hand-rolled prototype §6 calls for):
//
//   - "unshared": Select(A) and Select(B) each run their own independent
//     storage.Querier.Select call and fully decode their result (today's
//     behavior for two selectors that are not textually identical, so CSE
//     does not merge them).
//   - "shared_prototype": Select(A) runs once and is fully decoded into
//     memory; B's result is then derived by checking B's residual matcher
//     (the one matcher B has that A doesn't) against each of A's
//     already-decoded series, with no second Select call.
//
// The axes varied are exactly what §4 says the win depends on: A's
// absolute cardinality (numSeries), and B's cardinality as a fraction of
// A's (ratio, via the "half"/"decile"/"percentile" label each series
// carries). The crossover point — the ratio at which shared_prototype
// stops beating unshared, and how that shifts with numSeries — is the
// evidence §6 asks for.
func BenchmarkSubsetSelectorCandidate(b *testing.B) {
	const numPoints = 120 // 2 hours at 1-minute resolution: enough samples per series for chunk decode cost to show without dominating runtime.
	const interval = int64(60_000)

	// ratio labels: every series carries all three, so the same fixture
	// serves every ratio without needing separate data per ratio.
	ratios := []struct {
		name  string
		label string
		ratio float64
	}{
		{"ratio=0.50", "half", 0.5},
		{"ratio=0.10", "decile", 0.1},
		{"ratio=0.01", "percentile", 0.01},
	}

	for _, numSeries := range []int{100, 1000, 10000} {
		stor := teststorage.New(b)
		stor.DisableCompactions()
		if err := setupSSEBenchData(stor, numSeries, numPoints, interval); err != nil {
			b.Fatal(err)
		}

		aMatchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "__name__", "sse_test"),
			labels.MustNewMatcher(labels.MatchEqual, "job", "x"),
		}

		for _, r := range ratios {
			if r.ratio*float64(numSeries) < 1 {
				continue // Not enough series at this cardinality to realize the ratio.
			}
			residual := labels.MustNewMatcher(labels.MatchEqual, r.label, "0")
			bMatchers := append(append([]*labels.Matcher{}, aMatchers...), residual)

			b.Run(fmt.Sprintf("series=%d,%s", numSeries, r.name), func(b *testing.B) {
				b.Run("unshared", func(b *testing.B) {
					q, err := stor.Querier(0, int64(numPoints)*interval)
					if err != nil {
						b.Fatal(err)
					}
					defer q.Close()
					b.ReportAllocs()
					for b.Loop() {
						drainSelect(b, q, aMatchers)
						drainSelect(b, q, bMatchers)
					}
				})

				b.Run("shared_prototype", func(b *testing.B) {
					q, err := stor.Querier(0, int64(numPoints)*interval)
					if err != nil {
						b.Fatal(err)
					}
					defer q.Close()
					b.ReportAllocs()
					for b.Loop() {
						materialized := materializeSelect(b, q, aMatchers)
						filterMaterialized(materialized, residual)
					}
				})
			})
		}
	}
}

func setupSSEBenchData(stor *teststorage.TestStorage, numSeries, numPoints int, interval int64) error {
	ctx := context.Background()
	app := stor.Appender(ctx)
	for i := range numSeries {
		lbls := labels.FromStrings(
			"__name__", "sse_test",
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

// materializedSeries is what a consumer of a Select call ends up holding
// once it has fully read the result into memory: labels plus the complete
// decoded sample set, exactly the shape a real B-derivation would filter
// against.
type materializedSeries struct {
	lbls    labels.Labels
	samples []fSample
}

type fSample struct {
	t int64
	v float64
}

// drainSelect runs an independent Select and fully decodes every series'
// samples, discarding them. It stands in for a consumer that needs the
// selector's result but not for longer than one pass (mirrors "unshared"'s
// per-selector cost without benchmarking retention it doesn't need).
func drainSelect(b *testing.B, q storage.Querier, matchers []*labels.Matcher) {
	ss := q.Select(context.Background(), false, nil, matchers...)
	var it chunkenc.Iterator
	sink := 0.0
	for ss.Next() {
		s := ss.At()
		it = s.Iterator(it)
		for it.Next() != chunkenc.ValNone {
			_, v := it.At()
			sink += v
		}
	}
	if err := ss.Err(); err != nil {
		b.Fatal(err)
	}
	_ = sink
}

// materializeSelect runs Select once and fully decodes every series into
// memory, the "A fetched once, fully realized" half of shared_prototype.
func materializeSelect(b *testing.B, q storage.Querier, matchers []*labels.Matcher) []materializedSeries {
	ss := q.Select(context.Background(), false, nil, matchers...)
	var out []materializedSeries
	var it chunkenc.Iterator
	for ss.Next() {
		s := ss.At()
		it = s.Iterator(it)
		var samples []fSample
		for it.Next() != chunkenc.ValNone {
			t, v := it.At()
			samples = append(samples, fSample{t: t, v: v})
		}
		out = append(out, materializedSeries{lbls: s.Labels(), samples: samples})
	}
	if err := ss.Err(); err != nil {
		b.Fatal(err)
	}
	return out
}

// filterMaterialized derives B's result from A's already-materialized
// series by checking the residual matcher against each series' labels, with
// no storage access: the in-memory "filter-and-project" step
// docs/query-planner-phase4-design.md §3 describes in place of a second
// Select call.
func filterMaterialized(a []materializedSeries, residual *labels.Matcher) []materializedSeries {
	var out []materializedSeries
	for _, s := range a {
		if residual.Matches(s.lbls.Get(residual.Name)) {
			out = append(out, s)
		}
	}
	return out
}
