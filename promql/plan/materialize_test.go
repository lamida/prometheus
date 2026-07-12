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

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/plan"
)

// buildMaterializePlan parses+preprocesses query, strips
// plan.DeduplicateAndMergeNode markers, normalizes, and runs CSE over it — the
// full pipeline plan.MaterializeSharing expects to run after. It returns both
// the rewritten plan root and the (already preprocessed) real parser.Expr
// tree plan.FromExpr was built from, so a test can assert on the real tree after
// calling plan.MaterializeSharing.
func buildMaterializePlan(t *testing.T, query string) (plan.Node, parser.Expr) {
	t.Helper()
	expr := preprocess(t, query)
	p, err := plan.FromExpr(expr)
	require.NoError(t, err)
	root := plan.StripDeduplicateAndMergeMarkers(p.Root)
	plan.NormalizeForCSE(root)
	root, _ = plan.CommonSubexpressionElimination(root)
	return root, expr
}

func TestMaterializeSharing_BasicDuplicateSubtree(t *testing.T) {
	root, expr := buildMaterializePlan(t, "rate(foo[5m]) / rate(foo[5m])")

	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok, "expected *parser.BinaryExpr, got %T", expr)
	require.NotSame(t, be.LHS, be.RHS, "expected the real tree's two sides to still be distinct before materialization")

	refcounts := plan.MaterializeSharing(root)

	require.Same(t, be.LHS, be.RHS, "expected plan.MaterializeSharing to alias the real tree's LHS/RHS onto the same object")
	// The whole chain (rate -> matrix selector -> vector selector) merges,
	// so every level gets materialized, each with 2 eligible occurrences.
	require.Len(t, refcounts, 3)
	for _, count := range refcounts {
		require.Equal(t, 2, count)
	}
}

func TestMaterializeSharing_NoSharing_LeavesTreeAlone(t *testing.T) {
	root, expr := buildMaterializePlan(t, "foo / bar")

	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok)
	lhsBefore, rhsBefore := be.LHS, be.RHS

	refcounts := plan.MaterializeSharing(root)

	require.Empty(t, refcounts)
	require.Same(t, lhsBefore, be.LHS)
	require.Same(t, rhsBefore, be.RHS)
}

// TestMaterializeSharing_SubqueryBoundaryNeverAliased covers the exclusion
// scoping decision: a subexpression duplicated once at top level and once
// inside a subquery body must never be aliased together, even though the
// plan-level CSE pass (which doesn't know about execution boundaries) may
// still merge them into one plan plan.Node.
func TestMaterializeSharing_SubqueryBoundaryNeverAliased(t *testing.T) {
	root, expr := buildMaterializePlan(t, "rate(foo[5m]) + sum_over_time(rate(foo[5m])[10m:1m])")

	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok, "expected *parser.BinaryExpr, got %T", expr)
	lhsBefore := be.LHS

	refcounts := plan.MaterializeSharing(root)

	// The top-level rate(foo[5m]) must not have been rewritten to point at
	// (or been pointed at by) the copy living inside the subquery body: it
	// is the only eligible occurrence of its shape once the subquery-nested
	// occurrence is excluded, so nothing should be materialized for it.
	require.Same(t, lhsBefore, be.LHS)
	for canonical := range refcounts {
		require.NotSame(t, canonical, lhsBefore, "top-level occurrence must never be chosen as a materialization canonical when its only sharing partner is excluded")
	}
}

// TestMaterializeSharing_StepInvariantBoundaryNeverAliased covers the other
// exclusion: a subexpression duplicated once outside, and once inside, an
// @-pinned (step-invariant) subtree must never be aliased together.
func TestMaterializeSharing_StepInvariantBoundaryNeverAliased(t *testing.T) {
	root, expr := buildMaterializePlan(t, `rate(foo[5m]) + rate(foo[5m] @ 100)`)

	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok, "expected *parser.BinaryExpr, got %T", expr)
	lhsBefore := be.LHS

	refcounts := plan.MaterializeSharing(root)

	require.Same(t, lhsBefore, be.LHS)
	require.Empty(t, refcounts, "an @-pinned occurrence must never be materialized, even if the other side is otherwise identical")
}

// buildMaterializeSubsetPlan mirrors buildMaterializePlan, additionally
// running plan.SubsetSelectorElimination — the prerequisite
// plan.MaterializeSubsetSharing expects (see its doc).
func buildMaterializeSubsetPlan(t *testing.T, query string) (plan.Node, parser.Expr) {
	t.Helper()
	root, expr := buildMaterializePlan(t, query)
	plan.SubsetSelectorElimination(root)
	return root, expr
}

// TestMaterializeSubsetSharing_BasicSubsumption covers the direct case: a
// subsumed selector's real *parser.VectorSelector should map to its source's
// real *parser.VectorSelector, without touching the real tree itself (unlike
// plan.MaterializeSharing, this never aliases pointers — see
// plan.MaterializeSubsetSharing's doc).
func TestMaterializeSubsetSharing_BasicSubsumption(t *testing.T) {
	root, expr := buildMaterializeSubsetPlan(t, `up{job="a"} + up{job="a",env="prod"}`)

	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok, "expected *parser.BinaryExpr, got %T", expr)
	wide, ok := be.LHS.(*parser.VectorSelector)
	require.True(t, ok)
	narrow, ok := be.RHS.(*parser.VectorSelector)
	require.True(t, ok)
	require.Len(t, wide.LabelMatchers, 2)
	require.Len(t, narrow.LabelMatchers, 3)

	subsetSource := plan.MaterializeSubsetSharing(root)

	require.Len(t, subsetSource, 1)
	require.Same(t, wide, subsetSource[narrow], "expected the narrow selector's real expr to map to the wide selector's real expr")
	require.NotSame(t, be.LHS, be.RHS, "unlike CSE, the real tree's two selectors must remain distinct objects")
}

func TestMaterializeSubsetSharing_NoSubsumption_EmptyMap(t *testing.T) {
	root, _ := buildMaterializeSubsetPlan(t, `up{job="a"} + up{job="b"}`)

	subsetSource := plan.MaterializeSubsetSharing(root)
	require.Empty(t, subsetSource)
}

// TestMaterializeSubsetSharing_SubqueryBoundaryNeverMaterialized covers the
// same exclusion plan.MaterializeSharing applies: a subsumed selector, or
// its source, sitting only inside a subquery body must never be
// materialized into the map.
func TestMaterializeSubsetSharing_SubqueryBoundaryNeverMaterialized(t *testing.T) {
	root, _ := buildMaterializeSubsetPlan(t, `up{job="a"} + sum_over_time(up{job="a",env="prod"}[10m:1m])`)

	subsetSource := plan.MaterializeSubsetSharing(root)
	require.Empty(t, subsetSource, "the only subsumed occurrence sits inside a subquery body and must be excluded")
}

func TestStripDeduplicateAndMergeMarkers_RemovesEveryMarker(t *testing.T) {
	expr := preprocess(t, "foo or bar")
	p, err := plan.FromExpr(expr)
	require.NoError(t, err)

	// "or" always wraps its plan.BinaryExprNode in a plan.DeduplicateAndMergeNode
	// (see binaryExprNeedsDeduplicateAndMerge).
	_, ok := p.Root.(*plan.DeduplicateAndMergeNode)
	require.True(t, ok, "expected an \"or\" expression's root to be wrapped in plan.DeduplicateAndMergeNode before stripping")

	stripped := plan.StripDeduplicateAndMergeMarkers(p.Root)
	_, ok = stripped.(*plan.DeduplicateAndMergeNode)
	require.False(t, ok, "expected plan.DeduplicateAndMergeNode to be gone after stripping")

	require.Empty(t, findAll[*plan.DeduplicateAndMergeNode](stripped))
}

// TestStripDeduplicateAndMergeMarkers_NilRoot verifies the nil-root guard:
// callers should be able to pass a nil plan (e.g. the result of a failed
// upstream step propagated through) without a panic.
func TestStripDeduplicateAndMergeMarkers_NilRoot(t *testing.T) {
	require.Nil(t, plan.StripDeduplicateAndMergeMarkers(nil))
}
