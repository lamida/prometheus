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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/parser/posrange"
)

// preprocess parses query and runs it through parser.PreprocessExpr over a
// fixed test time range, matching how FromExpr's caller is expected to
// invoke it (see FromExpr's doc comment).
func preprocess(t *testing.T, query string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	require.NoError(t, err)
	start := time.Unix(0, 0)
	end := time.Unix(600, 0)
	preprocessed, err := promql.PreprocessExpr(expr, start, end, 15*time.Second)
	require.NoError(t, err)
	return preprocessed
}

func TestFromExpr_VectorSelector(t *testing.T) {
	expr := preprocess(t, "foo")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	vs, ok := p.Root.(*VectorSelectorNode)
	require.True(t, ok, "expected *VectorSelectorNode, got %T", p.Root)
	require.Equal(t, "foo", vs.Name)
	require.Equal(t, 0, vs.ChildCount())
	require.Equal(t, 0, vs.ParentCount())
}

func TestFromExpr_MatrixSelector(t *testing.T) {
	expr := preprocess(t, "foo[5m]")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	ms, ok := p.Root.(*MatrixSelectorNode)
	require.True(t, ok, "expected *MatrixSelectorNode, got %T", p.Root)
	require.Equal(t, 5*time.Minute, ms.Range)
	require.Equal(t, 1, ms.ChildCount())

	vs, ok := ms.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected child *VectorSelectorNode, got %T", ms.Child(0))
	require.Equal(t, "foo", vs.Name)
	require.Equal(t, 1, vs.ParentCount())
}

func TestFromExpr_BinaryExprBetweenSelectors(t *testing.T) {
	expr := preprocess(t, "foo + bar")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	be, ok := p.Root.(*BinaryExprNode)
	require.True(t, ok, "expected *BinaryExprNode, got %T", p.Root)
	require.Equal(t, 2, be.ChildCount())

	lhs, ok := be.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected LHS *VectorSelectorNode, got %T", be.Child(0))
	require.Equal(t, "foo", lhs.Name)

	rhs, ok := be.Child(1).(*VectorSelectorNode)
	require.True(t, ok, "expected RHS *VectorSelectorNode, got %T", be.Child(1))
	require.Equal(t, "bar", rhs.Name)
}

func TestFromExpr_AggregateWithGrouping(t *testing.T) {
	expr := preprocess(t, "sum by (job) (foo)")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	ae, ok := p.Root.(*AggregateExprNode)
	require.True(t, ok, "expected *AggregateExprNode, got %T", p.Root)
	require.Equal(t, []string{"job"}, ae.Grouping)
	require.False(t, ae.Without)
	require.False(t, ae.HasParam)
	require.Equal(t, 1, ae.ChildCount())

	vs, ok := ae.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected child *VectorSelectorNode, got %T", ae.Child(0))
	require.Equal(t, "foo", vs.Name)
}

func TestFromExpr_AggregateWithParam(t *testing.T) {
	expr := preprocess(t, "topk(5, foo)")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	ae, ok := p.Root.(*AggregateExprNode)
	require.True(t, ok, "expected *AggregateExprNode, got %T", p.Root)
	require.True(t, ae.HasParam)
	require.Equal(t, 2, ae.ChildCount())

	_, ok = ae.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected child 0 *VectorSelectorNode, got %T", ae.Child(0))
	_, ok = ae.Child(1).(*NumberLiteralNode)
	require.True(t, ok, "expected child 1 *NumberLiteralNode, got %T", ae.Child(1))
}

func TestFromExpr_NestedSubquery(t *testing.T) {
	expr := preprocess(t, "sum_over_time(foo[5m:1m])")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	// sum_over_time is a range-vector function other than last_over_time/
	// first_over_time, so FromExpr wraps it in a DeduplicateAndMergeNode
	// (see dedup.go's callNeedsDeduplicationWrap); unwrap it to get at the
	// CallNode this test is actually about.
	dedup, ok := p.Root.(*DeduplicateAndMergeNode)
	require.True(t, ok, "expected *DeduplicateAndMergeNode, got %T", p.Root)

	call, ok := dedup.Child(0).(*CallNode)
	require.True(t, ok, "expected *CallNode, got %T", dedup.Child(0))
	require.Equal(t, "sum_over_time", call.Func.Name)
	require.Equal(t, 1, call.ChildCount())

	sq, ok := call.Child(0).(*SubqueryExprNode)
	require.True(t, ok, "expected child *SubqueryExprNode, got %T", call.Child(0))
	require.Equal(t, 5*time.Minute, sq.Range)
	require.Equal(t, time.Minute, sq.Step)
	require.Equal(t, 1, sq.ChildCount())

	vs, ok := sq.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected grandchild *VectorSelectorNode, got %T", sq.Child(0))
	require.Equal(t, "foo", vs.Name)
}

func TestFromExpr_CallWithMultipleArgs(t *testing.T) {
	expr := preprocess(t, "clamp(foo, 0, 1)")
	p, err := FromExpr(expr)
	require.NoError(t, err)

	// clamp drops the __name__ label (see dedup.go's
	// callNeedsDeduplicationWrap), so FromExpr wraps it in a
	// DeduplicateAndMergeNode; unwrap it to get at the CallNode this test
	// is actually about.
	dedup, ok := p.Root.(*DeduplicateAndMergeNode)
	require.True(t, ok, "expected *DeduplicateAndMergeNode, got %T", p.Root)

	call, ok := dedup.Child(0).(*CallNode)
	require.True(t, ok, "expected *CallNode, got %T", dedup.Child(0))
	require.Equal(t, "clamp", call.Func.Name)
	require.Equal(t, 3, call.ChildCount())

	vs, ok := call.Child(0).(*VectorSelectorNode)
	require.True(t, ok, "expected arg 0 *VectorSelectorNode, got %T", call.Child(0))
	require.Equal(t, "foo", vs.Name)

	min, ok := call.Child(1).(*NumberLiteralNode)
	require.True(t, ok, "expected arg 1 *NumberLiteralNode, got %T", call.Child(1))
	require.InDelta(t, 0, min.Val, 0)

	max, ok := call.Child(2).(*NumberLiteralNode)
	require.True(t, ok, "expected arg 2 *NumberLiteralNode, got %T", call.Child(2))
	require.InDelta(t, 1, max.Val, 0)
}

// TestFromExpr_ParenExprUnwrapped verifies that a bare ParenExpr around a
// selector produces exactly the same plan shape as the unparenthesized
// query, i.e. ParenExpr contributes no node of its own.
func TestFromExpr_ParenExprUnwrapped(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr("(foo)")
	require.NoError(t, err)
	// Deliberately skip PreprocessExpr here: PreprocessExpr's own
	// "remove superfluous parenthesis" behavior (see its doc comment)
	// already strips some ParenExprs before FromExpr would ever see them.
	// Parsing directly guarantees the *parser.ParenExpr survives to
	// FromExpr, so this test exercises FromExpr's own unwrapping rather
	// than PreprocessExpr's.
	require.IsType(t, &parser.ParenExpr{}, expr)

	plan, err := FromExpr(expr)
	require.NoError(t, err)

	vs, ok := plan.Root.(*VectorSelectorNode)
	require.True(t, ok, "expected *VectorSelectorNode, got %T", plan.Root)
	require.Equal(t, "foo", vs.Name)
}

// TestFromExpr_StepInvariantExprUnwrapped verifies that a subexpression
// PreprocessExpr has wrapped in StepInvariantExpr (because it has no
// vector-selector-derived time dependence) is unwrapped by FromExpr,
// leaving no trace of the wrapper in the plan.
func TestFromExpr_StepInvariantExprUnwrapped(t *testing.T) {
	expr := preprocess(t, "vector(1) + foo")

	// Confirm PreprocessExpr actually produced a StepInvariantExpr here,
	// so this test is exercising what it claims to.
	be, ok := expr.(*parser.BinaryExpr)
	require.True(t, ok, "expected *parser.BinaryExpr, got %T", expr)
	require.IsType(t, &parser.StepInvariantExpr{}, be.LHS)

	p, err := FromExpr(expr)
	require.NoError(t, err)

	root, ok := p.Root.(*BinaryExprNode)
	require.True(t, ok, "expected *BinaryExprNode, got %T", p.Root)

	call, ok := root.Child(0).(*CallNode)
	require.True(t, ok, "expected LHS *CallNode (StepInvariantExpr unwrapped), got %T", root.Child(0))
	require.Equal(t, "vector", call.Func.Name)

	vs, ok := root.Child(1).(*VectorSelectorNode)
	require.True(t, ok, "expected RHS *VectorSelectorNode, got %T", root.Child(1))
	require.Equal(t, "foo", vs.Name)
}

// TestFromExpr_WholeExprStepInvariant covers the case where PreprocessExpr
// wraps the entire expression in a single top-level StepInvariantExpr.
func TestFromExpr_WholeExprStepInvariant(t *testing.T) {
	expr := preprocess(t, "1 + 1")
	require.IsType(t, &parser.StepInvariantExpr{}, expr)

	p, err := FromExpr(expr)
	require.NoError(t, err)

	root, ok := p.Root.(*BinaryExprNode)
	require.True(t, ok, "expected *BinaryExprNode, got %T", p.Root)
	require.Equal(t, 2, root.ChildCount())
	_, ok = root.Child(0).(*NumberLiteralNode)
	require.True(t, ok)
	_, ok = root.Child(1).(*NumberLiteralNode)
	require.True(t, ok)
}

// fakeUnhandledExpr is a synthetic parser.Expr implementation that FromExpr
// never handles, used only to exercise FromExpr's default error case. It is
// not a real query the parser produces.
type fakeUnhandledExpr struct{}

func (fakeUnhandledExpr) String() string    { return "fake" }
func (fakeUnhandledExpr) Pretty(int) string { return "fake" }
func (fakeUnhandledExpr) PositionRange() posrange.PositionRange {
	return posrange.PositionRange{}
}
func (fakeUnhandledExpr) Type() parser.ValueType { return parser.ValueTypeVector }
func (fakeUnhandledExpr) PromQLExpr()            {}

func TestFromExpr_UnhandledNodeType(t *testing.T) {
	_, err := FromExpr(fakeUnhandledExpr{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unhandled node type")
}
