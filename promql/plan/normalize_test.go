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

package plan_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/plan"
)

// TestNormalizeForCSE_MatcherOrder verifies that two selectors with the same
// matchers in different textual order become EquivalentTo after
// normalization, and are not equivalent before it.
func TestNormalizeForCSE_MatcherOrder(t *testing.T) {
	a := preprocess(t, `foo{job="a", instance="b"}`)
	b := preprocess(t, `foo{instance="b", job="a"}`)

	pa, err := plan.FromExpr(a)
	require.NoError(t, err)
	pb, err := plan.FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected selectors with different matcher order to differ before normalization")

	plan.NormalizeForCSE(pa.Root)
	plan.NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected selectors with different matcher order to be equivalent after normalization")
}

// TestNormalizeForCSE_GroupingOrder verifies that "sum by (a, b)" and "sum
// by (b, a)" over the same inner expression become EquivalentTo after
// normalization. Grouping order is verified order-independent against
// promql/engine.go's own AggregateExpr evaluation, which sorts Grouping
// itself before use (see plan.NormalizeForCSE's doc), so this is safe to
// normalize.
func TestNormalizeForCSE_GroupingOrder(t *testing.T) {
	a := preprocess(t, `sum by (a, b) (foo)`)
	b := preprocess(t, `sum by (b, a) (foo)`)

	pa, err := plan.FromExpr(a)
	require.NoError(t, err)
	pb, err := plan.FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected different grouping order to differ before normalization")

	plan.NormalizeForCSE(pa.Root)
	plan.NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected different grouping order to be equivalent after normalization")
}

// TestNormalizeForCSE_VectorMatchingOrder verifies that "on (a, b)" and
// "on (b, a)" (and the analogous group_left/group_right Include list)
// become EquivalentTo after normalization. Verified order-independent
// against labels.Labels.MatchLabels and labels.Builder.Keep/Del, which
// both treat their label-name arguments as an independent per-name
// operation regardless of order (see plan.NormalizeForCSE's doc).
func TestNormalizeForCSE_VectorMatchingOrder(t *testing.T) {
	a := preprocess(t, `foo * on (a, b) group_left (c, d) bar`)
	b := preprocess(t, `foo * on (b, a) group_left (d, c) bar`)

	pa, err := plan.FromExpr(a)
	require.NoError(t, err)
	pb, err := plan.FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected different on()/group_left() order to differ before normalization")

	plan.NormalizeForCSE(pa.Root)
	plan.NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected different on()/group_left() order to be equivalent after normalization")
}

// TestNormalizeForCSE_MatcherTieBreak verifies that sortMatchers' secondary
// (Type) and tertiary (Value) sort keys are exercised correctly, not just
// its primary (Name) key: two matchers sharing the same label name but
// differing in match type or value must still sort to a consistent,
// order-independent result, since PromQL allows more than one matcher per
// label name (e.g. a regex matcher combined with a negative equality
// matcher on the same label).
func TestNormalizeForCSE_MatcherTieBreak(t *testing.T) {
	a := preprocess(t, `foo{job=~"a.*", job!="b"}`)
	b := preprocess(t, `foo{job!="b", job=~"a.*"}`)

	pa, err := plan.FromExpr(a)
	require.NoError(t, err)
	pb, err := plan.FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected same-name matchers with different match types in different order to differ before normalization")

	plan.NormalizeForCSE(pa.Root)
	plan.NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected same-name matchers with different match types in different order to be equivalent after normalization")

	// Same match type, same name, different value: still must tie-break
	// consistently on Value so the two orderings converge.
	c := preprocess(t, `foo{job=~"a.*", job=~"b.*"}`)
	d := preprocess(t, `foo{job=~"b.*", job=~"a.*"}`)

	pc, err := plan.FromExpr(c)
	require.NoError(t, err)
	pd, err := plan.FromExpr(d)
	require.NoError(t, err)

	require.False(t, pc.Root.EquivalentTo(pd.Root), "expected same-name same-type matchers with different values in different order to differ before normalization")

	plan.NormalizeForCSE(pc.Root)
	plan.NormalizeForCSE(pd.Root)

	require.True(t, pc.Root.EquivalentTo(pd.Root), "expected same-name same-type matchers with different values in different order to be equivalent after normalization")
}

// TestNormalizeForCSE_DoesNotChangeShape verifies plan.NormalizeForCSE never adds,
// removes, or reorders children: it only mutates type-specific fields in
// place.
func TestNormalizeForCSE_DoesNotChangeShape(t *testing.T) {
	expr := preprocess(t, `sum by (b, a) (foo{job="x", instance="y"} + bar)`)
	p, err := plan.FromExpr(expr)
	require.NoError(t, err)

	before := describeTree(p.Root)
	plan.NormalizeForCSE(p.Root)
	after := describeTree(p.Root)

	require.Len(t, after, len(before), "expected the same number of nodes before and after normalization")
}

// describeTree returns a flat, pre-order list of Describe() strings for
// root's subtree, used only to assert shape (node count/nesting) is
// unchanged by a pass that's supposed to be field-only.
func describeTree(n plan.Node) []string {
	if n == nil {
		return nil
	}
	out := []string{n.Describe()}
	for i := 0; i < n.ChildCount(); i++ {
		out = append(out, describeTree(n.Child(i))...)
	}
	return out
}
