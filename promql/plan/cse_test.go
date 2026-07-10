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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/plan"
)

// buildAndPrepare parses+preprocesses query, builds its plan, and runs
// plan.NormalizeForCSE on it, matching the order plan.CommonSubexpressionElimination
// is meant to be called in (see plan.CommonSubexpressionElimination's doc).
func buildAndPrepare(t *testing.T, query string) plan.Node {
	t.Helper()
	expr := preprocess(t, query)
	p, err := plan.FromExpr(expr)
	require.NoError(t, err)
	plan.NormalizeForCSE(p.Root)
	return p.Root
}

// findAll returns every distinct (by pointer identity) node in root's
// subtree whose concrete type is T, in the order first encountered by a
// pre-order walk. A node reachable via more than one parent (already
// shared) is only included once.
func findAll[T plan.Node](root plan.Node) []T {
	var out []T
	seen := map[plan.Node]bool{}
	var walk func(n plan.Node)
	walk = func(n plan.Node) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		if t, ok := n.(T); ok {
			out = append(out, t)
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

// TestCommonSubexpressionElimination_BasicDuplicateSubtree covers the
// straightforward case: a query with an obviously duplicated subexpression
// on both sides of a binary operator should end up sharing one node.
func TestCommonSubexpressionElimination_BasicDuplicateSubtree(t *testing.T) {
	root := buildAndPrepare(t, "sum(rate(foo[5m])) / sum(rate(foo[5m]))")

	be, ok := root.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", root)
	require.NotSame(t, be.Child(0), be.Child(1), "expected the two sides to be distinct nodes before CSE")

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Positive(t, merged, "expected at least one node to be merged")

	be, ok = newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root after CSE, got %T", newRoot)

	lhs := be.Child(0)
	rhs := be.Child(1)
	require.Same(t, lhs, rhs, "expected both sides of / to point at the same shared node after CSE")
	require.Equal(t, 2, lhs.ParentCount(), "expected the shared node's ParentCount to reflect both parents")

	// Every node along the shared chain (sum -> DeduplicateAndMerge -> rate
	// -> matrix selector -> vector selector) should have been merged into
	// one instance: 5 levels, each duplicated twice pre-CSE, so exactly 5
	// merges (one per level) are expected.
	require.Equal(t, 5, merged, "expected exactly one merge per level of the shared chain")
}

// TestCommonSubexpressionElimination_ParentCountNotInflatedBelowSharedRoot is
// a regression test: canonicalize used to leave every node strictly beneath
// the top of a merged, multi-level-deep duplicate chain with a ParentCount
// one higher than reality. The bug was in the order of operations in
// canonicalize (cse.go): a node's own children are canonicalized and
// ReplaceChild'd (which increments their ParentCount to account for the
// edge from this node) BEFORE this node itself is checked against its
// bucket for being a duplicate. If this node then turns out to be a
// duplicate and is discarded in favor of an existing canonical survivor,
// nothing undid the ParentCount increments those ReplaceChild calls just
// made — so every child of a discarded node kept one permanent phantom
// parent, even though the discarded node itself is unreachable after
// merging and contributes no real edge to anything.
//
// "sum(rate(foo[5m])) / sum(rate(foo[5m]))" has a 5-level-deep shared chain
// (AggregateExpr -> DeduplicateAndMerge -> Call -> MatrixSelector ->
// VectorSelector, see TestCommonSubexpressionElimination_BasicDuplicateSubtree).
// Only the topmost node of that chain (the AggregateExprNode, reachable
// from both sides of the outer BinaryExprNode) genuinely has two parents
// after merging; every node below it in the now-singular chain has exactly
// one real parent (the node directly above it), and ParentCount() must
// reflect that, not the inflated value the bug produced.
func TestCommonSubexpressionElimination_ParentCountNotInflatedBelowSharedRoot(t *testing.T) {
	root := buildAndPrepare(t, "sum(rate(foo[5m])) / sum(rate(foo[5m]))")

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Equal(t, 5, merged, "expected exactly one merge per level of the shared chain")

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root after CSE, got %T", newRoot)
	require.Same(t, be.Child(0), be.Child(1), "expected both sides to share one node after CSE")
	require.Equal(t, 2, be.Child(0).ParentCount(), "expected the top of the shared chain to have exactly 2 parents")

	for _, n := range findAll[*plan.DeduplicateAndMergeNode](newRoot) {
		require.Equal(t, 1, n.ParentCount(), "expected the shared chain's DeduplicateAndMergeNode to have exactly 1 parent, not an inflated count")
	}
	for _, n := range findAll[*plan.CallNode](newRoot) {
		require.Equal(t, 1, n.ParentCount(), "expected the shared chain's rate() CallNode to have exactly 1 parent, not an inflated count")
	}
	for _, n := range findAll[*plan.MatrixSelectorNode](newRoot) {
		require.Equal(t, 1, n.ParentCount(), "expected the shared chain's MatrixSelectorNode to have exactly 1 parent, not an inflated count")
	}
	for _, n := range findAll[*plan.VectorSelectorNode](newRoot) {
		require.Equal(t, 1, n.ParentCount(), "expected the shared chain's VectorSelectorNode to have exactly 1 parent, not an inflated count")
	}
}

// TestCommonSubexpressionElimination_SharedLeafUnderDifferentParents covers
// merging a duplicated leaf subexpression that sits under otherwise
// different parents: abs(foo) and ceil(foo) don't merge themselves, but
// the inner "foo" selector they both reference should still merge into one
// shared plan.VectorSelectorNode.
func TestCommonSubexpressionElimination_SharedLeafUnderDifferentParents(t *testing.T) {
	root := buildAndPrepare(t, "abs(foo) + ceil(foo)")

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Positive(t, merged)

	selectors := findAll[*plan.VectorSelectorNode](newRoot)
	require.Len(t, selectors, 1, "expected the two \"foo\" selectors to merge into one shared node")
	require.Equal(t, 2, selectors[0].ParentCount())

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", newRoot)
	require.NotSame(t, be.Child(0), be.Child(1), "expected abs(foo) and ceil(foo) themselves to remain distinct nodes")
}

// TestCommonSubexpressionElimination_TwoNumberLiterals covers merging two
// occurrences of the exact same scalar literal appearing twice in one
// query: a legitimate, simple win that should not be special-cased away
// just because both nodes are leaves.
func TestCommonSubexpressionElimination_TwoNumberLiterals(t *testing.T) {
	root := buildAndPrepare(t, "5 + 5")

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Equal(t, 1, merged)

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", newRoot)
	require.Same(t, be.Child(0), be.Child(1))
	require.Equal(t, 2, be.Child(0).ParentCount())
}

// TestCommonSubexpressionElimination_DifferentMatchersDoNotMerge covers
// non-equivalence: two selectors with different matchers must not merge.
func TestCommonSubexpressionElimination_DifferentMatchersDoNotMerge(t *testing.T) {
	root := buildAndPrepare(t, `foo{job="a"} + foo{job="b"}`)

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Equal(t, 0, merged)

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", newRoot)
	require.NotSame(t, be.Child(0), be.Child(1))
}

// TestCommonSubexpressionElimination_DifferentRangesDoNotMerge covers
// non-equivalence for plan.MatrixSelectorNode.Range: two matrix selectors over
// the same metric but different ranges must not merge, even though their
// inner plan.VectorSelectorNode (same metric, no offset/matchers difference)
// legitimately does.
func TestCommonSubexpressionElimination_DifferentRangesDoNotMerge(t *testing.T) {
	root := buildAndPrepare(t, "sum_over_time(foo[5m]) + sum_over_time(foo[10m])")

	newRoot, merged := plan.CommonSubexpressionElimination(root)

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", newRoot)
	require.NotSame(t, be.Child(0), be.Child(1), "expected the two differently-ranged sum_over_time chains to remain distinct")

	unwrap := func(n plan.Node) *plan.MatrixSelectorNode {
		dedup, ok := n.(*plan.DeduplicateAndMergeNode)
		require.True(t, ok, "expected *plan.DeduplicateAndMergeNode, got %T", n)
		call, ok := dedup.Child(0).(*plan.CallNode)
		require.True(t, ok, "expected *plan.CallNode, got %T", dedup.Child(0))
		ms, ok := call.Child(0).(*plan.MatrixSelectorNode)
		require.True(t, ok, "expected *plan.MatrixSelectorNode, got %T", call.Child(0))
		return ms
	}
	ms0 := unwrap(be.Child(0))
	ms1 := unwrap(be.Child(1))
	require.NotEqual(t, ms0.Range, ms1.Range)

	// The inner "foo" vector selectors are identical (no matchers/offset
	// difference), so they should still have merged into one shared node
	// even though the enclosing matrix selectors (and everything above
	// them, transitively, since their children now differ) did not.
	require.Same(t, ms0.Child(0), ms1.Child(0), "expected the shared inner selector to merge despite the differing outer range")
	require.Equal(t, 1, merged)
}

// TestCommonSubexpressionElimination_DifferentOffsetsDoNotMerge covers
// non-equivalence for plan.VectorSelectorNode.Offset. preprocess (this
// package's test helper) never calls promql/engine.go's unexported
// setOffsetForAtModifier — see plan.FromExpr's doc comment on why that call,
// not just parser.PreprocessExpr, is what actually populates .Offset — so
// a real "foo offset 1m" parsed and preprocessed by this test's helper
// still has plan.VectorSelectorNode.Offset == 0. To exercise Offset-based
// non-equivalence directly, this test instead sets Offset by hand on
// already-built nodes, which is the direct approach for a field this
// package's own exported API lets a caller set without needing engine.go.
func TestCommonSubexpressionElimination_DifferentOffsetsDoNotMerge(t *testing.T) {
	root := buildAndPrepare(t, "foo + foo")
	be, ok := root.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", root)

	lhs, ok := be.Child(0).(*plan.VectorSelectorNode)
	require.True(t, ok)
	rhs, ok := be.Child(1).(*plan.VectorSelectorNode)
	require.True(t, ok)
	lhs.Offset = time.Minute
	rhs.Offset = 2 * time.Minute

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Equal(t, 0, merged)

	be, ok = newRoot.(*plan.BinaryExprNode)
	require.True(t, ok)
	require.NotSame(t, be.Child(0), be.Child(1))
}

// TestCommonSubexpressionElimination_AtModifierUnderSubqueryNeverMerges
// covers the deliberate v1 conservatism: a plan.VectorSelectorNode carrying its
// own @ modifier, sitting anywhere underneath a plan.SubqueryExprNode, must
// never be considered equivalent to anything, including a
// structurally-identical twin. See plan.VectorSelectorNode.hasUnstableOffset's
// doc and docs/query-planner-phase2-design.md §3, Open Question 1.
func TestCommonSubexpressionElimination_AtModifierUnderSubqueryNeverMerges(t *testing.T) {
	root := buildAndPrepare(t, "sum_over_time((foo @ 100)[5m:1m]) + sum_over_time((foo @ 100)[5m:1m])")

	selectorsBefore := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectorsBefore, 2, "expected two distinct selector instances before CSE")
	require.True(t, selectorsBefore[0].HasUnstableOffset(), "expected the @-under-subquery selector to be flagged unstable")
	require.True(t, selectorsBefore[1].HasUnstableOffset())

	// Even though the two selectors are otherwise byte-for-byte identical
	// (same name, matchers, offset, timestamp), EquivalentTo must refuse to
	// call them equivalent.
	require.False(t, selectorsBefore[0].EquivalentTo(selectorsBefore[1]))

	newRoot, merged := plan.CommonSubexpressionElimination(root)

	selectorsAfter := findAll[*plan.VectorSelectorNode](newRoot)
	require.Len(t, selectorsAfter, 2, "expected the two @-under-subquery selectors to remain distinct after CSE")
	require.Equal(t, 1, selectorsAfter[0].ParentCount())
	require.Equal(t, 1, selectorsAfter[1].ParentCount())

	// Nothing else in the tree duplicates (the two sum_over_time/subquery
	// chains only share the leaf selector, which is exactly what must not
	// merge), so no merges at all should have happened.
	require.Equal(t, 0, merged)
}

// TestCommonSubexpressionElimination_AtModifierWithoutSubqueryDoesMerge
// covers the same textual selector (its own @ modifier) as the previous
// test, but NOT nested under any subquery, to prove the restriction is
// scoped to the subquery-nesting case specifically, not a blanket "any @
// modifier disables CSE".
func TestCommonSubexpressionElimination_AtModifierWithoutSubqueryDoesMerge(t *testing.T) {
	root := buildAndPrepare(t, "(foo @ 100) + (foo @ 100)")

	selectorsBefore := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectorsBefore, 2)
	require.False(t, selectorsBefore[0].HasUnstableOffset(), "expected the @ selector to be stable when not nested under a subquery")
	require.False(t, selectorsBefore[1].HasUnstableOffset())

	newRoot, merged := plan.CommonSubexpressionElimination(root)
	require.Equal(t, 1, merged)

	selectorsAfter := findAll[*plan.VectorSelectorNode](newRoot)
	require.Len(t, selectorsAfter, 1, "expected the two @ selectors to merge into one shared node")
	require.Equal(t, 2, selectorsAfter[0].ParentCount())
}

// TestCommonSubexpressionElimination_DifferentSubqueryRangesDoNotMerge
// exercises a realistic, non-@-specific path-dependence case: two
// sum_over_time subqueries over the same selector but with different
// subquery ranges must not merge at the subquery/call level, even though
// their shared inner selector legitimately does. This is the general
// path-dependence concern §3 raises, distinct from the @-under-subquery
// conservatism covered above.
func TestCommonSubexpressionElimination_DifferentSubqueryRangesDoNotMerge(t *testing.T) {
	root := buildAndPrepare(t, "sum_over_time(foo[5m:1m]) + sum_over_time(foo[10m:1m])")

	newRoot, merged := plan.CommonSubexpressionElimination(root)

	be, ok := newRoot.(*plan.BinaryExprNode)
	require.True(t, ok, "expected *plan.BinaryExprNode root, got %T", newRoot)
	require.NotSame(t, be.Child(0), be.Child(1), "expected the two differently-ranged subquery chains to remain distinct")

	subqueries := findAll[*plan.SubqueryExprNode](newRoot)
	require.Len(t, subqueries, 2, "expected the two SubqueryExprNodes (different ranges) to remain distinct")

	selectors := findAll[*plan.VectorSelectorNode](newRoot)
	require.Len(t, selectors, 1, "expected the shared inner \"foo\" selector to merge despite the differing subquery ranges")
	require.Equal(t, 2, selectors[0].ParentCount())
	require.Positive(t, merged)
}
