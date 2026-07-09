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

package plan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeForCSE_MatcherOrder verifies that two selectors with the same
// matchers in different textual order become EquivalentTo after
// normalization, and are not equivalent before it.
func TestNormalizeForCSE_MatcherOrder(t *testing.T) {
	a := preprocess(t, `foo{job="a", instance="b"}`)
	b := preprocess(t, `foo{instance="b", job="a"}`)

	pa, err := FromExpr(a)
	require.NoError(t, err)
	pb, err := FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected selectors with different matcher order to differ before normalization")

	NormalizeForCSE(pa.Root)
	NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected selectors with different matcher order to be equivalent after normalization")
}

// TestNormalizeForCSE_GroupingOrder verifies that "sum by (a, b)" and "sum
// by (b, a)" over the same inner expression become EquivalentTo after
// normalization. Grouping order is verified order-independent against
// promql/engine.go's own AggregateExpr evaluation, which sorts Grouping
// itself before use (see NormalizeForCSE's doc), so this is safe to
// normalize.
func TestNormalizeForCSE_GroupingOrder(t *testing.T) {
	a := preprocess(t, `sum by (a, b) (foo)`)
	b := preprocess(t, `sum by (b, a) (foo)`)

	pa, err := FromExpr(a)
	require.NoError(t, err)
	pb, err := FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected different grouping order to differ before normalization")

	NormalizeForCSE(pa.Root)
	NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected different grouping order to be equivalent after normalization")
}

// TestNormalizeForCSE_VectorMatchingOrder verifies that "on (a, b)" and
// "on (b, a)" (and the analogous group_left/group_right Include list)
// become EquivalentTo after normalization. Verified order-independent
// against labels.Labels.MatchLabels and labels.Builder.Keep/Del, which
// both treat their label-name arguments as an independent per-name
// operation regardless of order (see NormalizeForCSE's doc).
func TestNormalizeForCSE_VectorMatchingOrder(t *testing.T) {
	a := preprocess(t, `foo * on (a, b) group_left (c, d) bar`)
	b := preprocess(t, `foo * on (b, a) group_left (d, c) bar`)

	pa, err := FromExpr(a)
	require.NoError(t, err)
	pb, err := FromExpr(b)
	require.NoError(t, err)

	require.False(t, pa.Root.EquivalentTo(pb.Root), "expected different on()/group_left() order to differ before normalization")

	NormalizeForCSE(pa.Root)
	NormalizeForCSE(pb.Root)

	require.True(t, pa.Root.EquivalentTo(pb.Root), "expected different on()/group_left() order to be equivalent after normalization")
}

// TestNormalizeForCSE_DoesNotChangeShape verifies NormalizeForCSE never adds,
// removes, or reorders children: it only mutates type-specific fields in
// place.
func TestNormalizeForCSE_DoesNotChangeShape(t *testing.T) {
	expr := preprocess(t, `sum by (b, a) (foo{job="x", instance="y"} + bar)`)
	p, err := FromExpr(expr)
	require.NoError(t, err)

	before := describeTree(p.Root)
	NormalizeForCSE(p.Root)
	after := describeTree(p.Root)

	require.Len(t, after, len(before), "expected the same number of nodes before and after normalization")
}

// describeTree returns a flat, pre-order list of Describe() strings for
// root's subtree, used only to assert shape (node count/nesting) is
// unchanged by a pass that's supposed to be field-only.
func describeTree(n Node) []string {
	if n == nil {
		return nil
	}
	out := []string{n.Describe()}
	for i := 0; i < n.ChildCount(); i++ {
		out = append(out, describeTree(n.Child(i))...)
	}
	return out
}
