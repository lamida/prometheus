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

package promql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/util/teststorage"
)

// TestSSESharedNode_ReleasesPoolExactlyOnce is the SSE counterpart to
// TestCSESharedNode_InstantQueryStillReleasesPool: it exercises
// shouldReleaseSharedNode's `if src, ok := ev.subsetSource[expr]; ok {
// return ev.shouldReleaseSharedNode(src) }` redirection (promql/engine.go)
// through a real query, not just a correctness check.
//
// "foo{a=\"1\"} + foo{a=\"1\",b=\"2\"}" makes foo{a="1",b="2"} a
// subset-derived dependent of foo{a="1"}: MaterializeSubsetSharing records
// foo{a="1"} as the source with a refcount of 2 (its own natural
// occurrence, plus one for the dependent — see the "Its own natural
// occurrence" comment in newPlanOptimizationState/materializePlanOptimizations).
// The source itself matches two series (the plain a="1" one, and the
// a="1",b="2" one), each with its own pooled point slice; the dependent
// only matches the second. If release used the dependent's own (locally
// filtered, single-series) view instead of the source's full one, the
// first series' point slice would never make it back to the pool — a
// silent leak, not a double-release, but still exactly the kind of gap an
// adversarial pool-release test is meant to catch (this test previously
// asserted `released == 1`, which the leaky behavior also satisfied;
// asserting the true expected count of 2 is what actually catches it).
func TestSSESharedNode_ReleasesPoolExactlyOnce(t *testing.T) {
	stor := teststorage.New(t)
	app := stor.Appender(context.Background())
	series := labels.FromStrings(labels.MetricName, "foo", "a", "1")
	_, err := app.Append(0, series, 0, 42)
	require.NoError(t, err)
	seriesB := labels.FromStrings(labels.MetricName, "foo", "a", "1", "b", "2")
	_, err = app.Append(0, seriesB, 0, 7)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	engine := NewEngine(EngineOpts{
		MaxSamples:                      50000000,
		Timeout:                         100 * time.Second,
		EnableSubsetSelectorElimination: true,
	})

	// Drain whatever is already sitting in the shared, package-level pools
	// from other tests in this process, so this test's own count below only
	// reflects what this query itself puts back.
	drainFPointPool()

	ctx := context.Background()
	q, err := engine.NewInstantQuery(ctx, stor, nil, `foo{a="1"} + foo{a="1",b="2"}`, time.Unix(0, 0))
	require.NoError(t, err)
	defer q.Close()

	res := q.Exec(ctx)
	require.NoError(t, res.Err)

	released := drainFPointPool()
	require.Equal(t, 2, released, "expected both of the shared source selector's series' point slices to be released back to fPointPool exactly once each, not leaked and not double-released")
}

// TestSSESharedNode_CombinedWithCSE_ReleasesPoolExactlyOnce covers the
// "combined with CSE" gap the phase 5 punch list calls out: a selector that
// is both a real CSE duplicate (two literal AST occurrences of
// foo{a="1"}) and an SSE source (a third occurrence, foo{a="1",b="2"},
// depends on it via subset elimination) must still release its own two
// series' pooled point slices exactly once overall, even though two
// independent mechanisms (CSE's own refcount and SSE's subsetSource
// redirection) both contribute to its total refcount. The inner
// "foo{a=\"1\"} + foo{a=\"1\"}" sub-expression's own (unshared) binary-op
// result contributes two more point slices of its own, for four expected
// releases in total.
func TestSSESharedNode_CombinedWithCSE_ReleasesPoolExactlyOnce(t *testing.T) {
	stor := teststorage.New(t)
	app := stor.Appender(context.Background())
	series := labels.FromStrings(labels.MetricName, "foo", "a", "1")
	_, err := app.Append(0, series, 0, 42)
	require.NoError(t, err)
	seriesB := labels.FromStrings(labels.MetricName, "foo", "a", "1", "b", "2")
	_, err = app.Append(0, seriesB, 0, 7)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	engine := NewEngine(EngineOpts{
		MaxSamples:                           50000000,
		Timeout:                              100 * time.Second,
		EnableCommonSubexpressionElimination: true,
		EnableSubsetSelectorElimination:      true,
	})

	drainFPointPool()

	ctx := context.Background()
	q, err := engine.NewInstantQuery(ctx, stor, nil, `foo{a="1"} + foo{a="1"} + foo{a="1",b="2"}`, time.Unix(0, 0))
	require.NoError(t, err)
	defer q.Close()

	res := q.Exec(ctx)
	require.NoError(t, res.Err)

	released := drainFPointPool()
	require.Equal(t, 4, released, "expected the CSE-and-SSE-shared source's two series to be released exactly once each (2), plus the inner binary op's own unshared result (2), not once per contributing sharing mechanism and not leaked")
}
