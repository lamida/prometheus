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

// TestCSESharedNode_InstantQueryStillReleasesPool is a regression test for a
// bug found while adversarially reviewing this package's CSE wiring: rangeEval's
// "instant query" shortcut (the `if ev.endTimestamp == ev.startTimestamp`
// branch) returns before reaching the general release loop
// (releaseOrigMatrixes), so for an instant query, a shared node's pooled
// point slices were never returned to fPointPool/hPointPool at all — not a
// double-release or a wrong result (Go's GC still reclaims the memory), but
// a missed-reuse regression that defeats part of the point of Strategy A
// for the very common case of instant queries.
//
// "foo + foo" is a single-level self-duplication: after CSE, both operands
// of "+" are materialized to the same *parser.VectorSelector, with a
// refcount of 2, and both release attempts happen within one rangeEval
// call's origMatrixes loop — this isolates the instant-query-shortcut fix
// from the separate, still-open nested-sharing limitation documented on
// shouldReleaseSharedNode (which affects multi-level shares, e.g.
// "abs(foo)+abs(foo)", not this single-level case).
func TestCSESharedNode_InstantQueryStillReleasesPool(t *testing.T) {
	stor := teststorage.New(t)
	app := stor.Appender(context.Background())
	series := labels.FromStrings(labels.MetricName, "foo")
	_, err := app.Append(0, series, 0, 42)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	engine := NewEngine(EngineOpts{
		MaxSamples:                           50000000,
		Timeout:                              100 * time.Second,
		EnableCommonSubexpressionElimination: true,
	})

	// Drain whatever is already sitting in the shared, package-level pools
	// from other tests in this process, so this test's own count below only
	// reflects what this query itself puts back.
	drainFPointPool()

	ctx := context.Background()
	q, err := engine.NewInstantQuery(ctx, stor, nil, "foo + foo", time.Unix(0, 0))
	require.NoError(t, err)
	defer q.Close()

	res := q.Exec(ctx)
	require.NoError(t, res.Err)

	released := drainFPointPool()
	require.Positive(t, released, "expected the shared \"foo\" selector's point slice to be released back to fPointPool for an instant query, same as it would be for a range query")
}

// TestCSESharedNode_NestedSharingResultsStayCorrect documents a known,
// intentional limitation of shouldReleaseSharedNode (see its doc comment):
// for a multi-level shared subtree, such as "abs(foo)+abs(foo)" (the
// CallNode, and the VectorSelectorNode beneath it, are both independently
// tracked shared nodes), the descendant's pooled point slices are not
// returned to the pool this query's lifetime, because the ancestor's
// second occurrence is served from cache and never redescends to make a
// release attempt on its child. This is a missed-reuse performance
// regression, not a correctness bug — this test exists to nail down the
// "not a correctness bug" half of that claim: the query's actual result
// must still be correct even though the pool-release accounting is
// imperfect underneath it.
func TestCSESharedNode_NestedSharingResultsStayCorrect(t *testing.T) {
	stor := teststorage.New(t)
	app := stor.Appender(context.Background())
	series := labels.FromStrings(labels.MetricName, "foo")
	_, err := app.Append(0, series, 0, 42)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	engineNoCSE := NewEngine(EngineOpts{MaxSamples: 50000000, Timeout: 100 * time.Second})
	engineCSE := NewEngine(EngineOpts{MaxSamples: 50000000, Timeout: 100 * time.Second, EnableCommonSubexpressionElimination: true})

	ctx := context.Background()
	const query = "abs(foo) + abs(foo)"

	qNoCSE, err := engineNoCSE.NewInstantQuery(ctx, stor, nil, query, time.Unix(0, 0))
	require.NoError(t, err)
	defer qNoCSE.Close()
	resNoCSE := qNoCSE.Exec(ctx)
	require.NoError(t, resNoCSE.Err)

	qCSE, err := engineCSE.NewInstantQuery(ctx, stor, nil, query, time.Unix(0, 0))
	require.NoError(t, err)
	defer qCSE.Close()
	resCSE := qCSE.Exec(ctx)
	require.NoError(t, resCSE.Err)

	require.Equal(t, resNoCSE.Value.String(), resCSE.Value.String(), "expected identical results with and without CSE, even though pool-release accounting is known to be imperfect for this nested-sharing shape")
}

// drainFPointPool repeatedly calls getFPointSlice until the pool reports
// empty (a nil/zero-cap result), returning how many non-empty slices were
// drained. zeropool.Pool is backed by sync.Pool, so this is only reliable
// within a single fast test with no intervening GC — which is the only
// thing this test needs.
func drainFPointPool() int {
	n := 0
	for {
		p := getFPointSlice(0)
		if cap(p) == 0 {
			return n
		}
		n++
	}
}
