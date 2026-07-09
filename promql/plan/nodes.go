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
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// VectorSelectorNode selects a Vector of series, mirroring parser.VectorSelector.
type VectorSelectorNode struct {
	baseNode

	// Name is the metric name being selected, or "" if the selector has no
	// name matcher (e.g. `{job="x"}`).
	Name string
	// LabelMatchers are the matchers restricting which series are selected.
	LabelMatchers []*labels.Matcher
	// Offset is the fully resolved offset to apply during evaluation, as
	// computed by parser.PreprocessExpr. See
	// docs/query-planner-phase2-design.md §3 for why this must be the
	// resolved value, not a path-relative one re-derived later.
	Offset time.Duration
	// Timestamp is the fully resolved @ timestamp, or nil if none was set.
	Timestamp *int64
	// SkipHistogramBuckets mirrors parser.VectorSelector.SkipHistogramBuckets.
	SkipHistogramBuckets bool

	// hasUnstableOffset is set by FromExpr when this selector carries its
	// own @ modifier (Timestamp != nil) AND sits anywhere underneath a
	// SubqueryExprNode. In that case Offset, though a concrete
	// time.Duration value, is not a stable, evaluation-time-independent
	// fact about this node: setOffsetForAtModifier (promql/engine.go:4639)
	// recomputes it fresh on every subquery iteration, using that
	// iteration's own runtime evalTime (see evalSubquery,
	// engine.go:2005), and no single resolved value snapshotted once at
	// plan-build time can soundly stand in for all of those iterations. A
	// selector without its own @ modifier never has this problem: its
	// OriginalOffset is nesting-independent regardless of how many
	// subqueries it sits under (see getOffset's `if ts == nil { return
	// originalOffset }` case inside setOffsetForAtModifier). CSE must
	// never consider two such nodes equivalent — see EquivalentTo below —
	// as a deliberate v1 conservatism, not a bug: this is the exact case
	// docs/query-planner-phase2-design.md §3's Open Question 1 flags as
	// genuinely uncertain, and the decision is to never share these nodes
	// rather than try to resolve the uncertainty now.
	hasUnstableOffset bool
}

// ChildCount always returns 0: a vector selector is a leaf node.
func (n *VectorSelectorNode) ChildCount() int { return 0 }

// EquivalentTo reports whether other is a VectorSelectorNode selecting the
// same name, matchers, offset, timestamp, and histogram-bucket-skipping
// behavior as n. If either n or other has hasUnstableOffset set,
// EquivalentTo unconditionally returns false: see hasUnstableOffset's doc.
func (n *VectorSelectorNode) EquivalentTo(other Node) bool {
	o, ok := other.(*VectorSelectorNode)
	if !ok {
		return false
	}
	if n.hasUnstableOffset || o.hasUnstableOffset {
		return false
	}
	return n.Name == o.Name &&
		n.Offset == o.Offset &&
		equalInt64Ptr(n.Timestamp, o.Timestamp) &&
		n.SkipHistogramBuckets == o.SkipHistogramBuckets &&
		equalMatchers(n.LabelMatchers, o.LabelMatchers)
}

// Describe returns a short summary of this node for debugging.
func (n *VectorSelectorNode) Describe() string {
	return fmt.Sprintf("VectorSelector(name=%q, matchers=%d, offset=%s, timestamp=%s)",
		n.Name, len(n.LabelMatchers), n.Offset, formatInt64Ptr(n.Timestamp))
}

// HasUnstableOffset reports whether n was built from a VectorSelector
// with its own @ modifier that sits underneath a SubqueryExprNode,
// making its Offset evaluation-time-dependent (see hasUnstableOffset's
// doc). EquivalentTo unconditionally treats such a node as never
// equivalent to any other node, so CommonSubexpressionElimination never
// merges it. Exported primarily so tests outside this package can assert
// on this fact directly.
func (n *VectorSelectorNode) HasUnstableOffset() bool {
	return n.hasUnstableOffset
}

// MatrixSelectorNode selects a Matrix of series over a time range, mirroring
// parser.MatrixSelector. Its single child is the VectorSelectorNode it
// ranges over.
type MatrixSelectorNode struct {
	baseNode

	// Range is the fully resolved lookback range for this matrix selector.
	Range time.Duration
}

// EquivalentTo reports whether other is a MatrixSelectorNode with the same
// range as n. It does not compare children; see the Node.EquivalentTo doc.
func (n *MatrixSelectorNode) EquivalentTo(other Node) bool {
	o, ok := other.(*MatrixSelectorNode)
	if !ok {
		return false
	}
	return n.Range == o.Range
}

// Describe returns a short summary of this node for debugging.
func (n *MatrixSelectorNode) Describe() string {
	return fmt.Sprintf("MatrixSelector(range=%s)", n.Range)
}

// BinaryExprNode represents a binary operation between two child nodes,
// mirroring parser.BinaryExpr.
type BinaryExprNode struct {
	baseNode

	// Op is the operation of the expression.
	Op parser.ItemType
	// VectorMatching describes how the two operands are matched, or nil if
	// both operands are scalars.
	VectorMatching *parser.VectorMatching
	// ReturnBool is set for a comparison operator that returns 0/1 rather
	// than filtering.
	ReturnBool bool
}

// EquivalentTo reports whether other is a BinaryExprNode with the same
// operator, vector-matching behavior, and ReturnBool as n.
func (n *BinaryExprNode) EquivalentTo(other Node) bool {
	o, ok := other.(*BinaryExprNode)
	if !ok {
		return false
	}
	return n.Op == o.Op &&
		n.ReturnBool == o.ReturnBool &&
		equalVectorMatching(n.VectorMatching, o.VectorMatching)
}

// Describe returns a short summary of this node for debugging.
func (n *BinaryExprNode) Describe() string {
	return fmt.Sprintf("BinaryExpr(op=%s, returnBool=%t)", n.Op, n.ReturnBool)
}

// AggregateExprNode represents an aggregation over a child node, mirroring
// parser.AggregateExpr. Its children are its expression (index 0) followed
// by its parameter (index 1), when a parameter is present.
type AggregateExprNode struct {
	baseNode

	// Op is the aggregation operation.
	Op parser.ItemType
	// Grouping is the set of labels to group by (or drop, if Without is
	// set).
	Grouping []string
	// Without indicates Grouping lists labels to drop rather than keep.
	Without bool
	// HasParam records whether this aggregation had a Param expression
	// (e.g. topk's k, or count_values' label name), so that ChildCount can
	// distinguish a one-child from a two-child aggregation without
	// inspecting the children slice's contents.
	HasParam bool
}

// EquivalentTo reports whether other is an AggregateExprNode with the same
// operation and grouping behavior as n. Grouping is compared in order
// (slices.Equal); NormalizeForCSE (normalize.go) sorts it once, up front,
// so this order-sensitive comparison still produces the semantically
// correct order-independent answer — see NormalizeForCSE's doc for why
// Grouping's order is semantically irrelevant (promql/engine.go sorts it
// itself before use).
func (n *AggregateExprNode) EquivalentTo(other Node) bool {
	o, ok := other.(*AggregateExprNode)
	if !ok {
		return false
	}
	return n.Op == o.Op &&
		n.Without == o.Without &&
		n.HasParam == o.HasParam &&
		slices.Equal(n.Grouping, o.Grouping)
}

// Describe returns a short summary of this node for debugging.
func (n *AggregateExprNode) Describe() string {
	return fmt.Sprintf("AggregateExpr(op=%s, without=%t, grouping=%v)", n.Op, n.Without, n.Grouping)
}

// CallNode represents a function call, mirroring parser.Call. Its children
// are the call's arguments, in order.
type CallNode struct {
	baseNode

	// Func is the function being called.
	Func *parser.Function
}

// EquivalentTo reports whether other is a CallNode calling the same
// function as n.
func (n *CallNode) EquivalentTo(other Node) bool {
	o, ok := other.(*CallNode)
	if !ok {
		return false
	}
	if n.Func == nil || o.Func == nil {
		return n.Func == o.Func
	}
	return n.Func.Name == o.Func.Name
}

// Describe returns a short summary of this node for debugging.
func (n *CallNode) Describe() string {
	name := "<nil>"
	if n.Func != nil {
		name = n.Func.Name
	}
	return fmt.Sprintf("Call(func=%s)", name)
}

// SubqueryExprNode represents a subquery, mirroring parser.SubqueryExpr. Its
// single child is the expression being subqueried.
type SubqueryExprNode struct {
	baseNode

	// Range is the subquery's range.
	Range time.Duration
	// Offset is the fully resolved offset to apply during evaluation.
	Offset time.Duration
	// Timestamp is the fully resolved @ timestamp, or nil if none was set.
	Timestamp *int64
	// Step is the subquery's resolved step.
	Step time.Duration

	// hasUnstableOffset is set by FromExpr when this subquery carries its
	// own @ modifier (Timestamp != nil) AND sits underneath another,
	// enclosing SubqueryExprNode. See VectorSelectorNode.hasUnstableOffset
	// for the full reasoning: the same nesting-dependent recomputation by
	// setOffsetForAtModifier applies here, since *parser.SubqueryExpr is
	// one of the three node types getOffset handles.
	hasUnstableOffset bool
}

// EquivalentTo reports whether other is a SubqueryExprNode with the same
// range, offset, timestamp, and step as n. If either n or other has
// hasUnstableOffset set, EquivalentTo unconditionally returns false: see
// hasUnstableOffset's doc.
func (n *SubqueryExprNode) EquivalentTo(other Node) bool {
	o, ok := other.(*SubqueryExprNode)
	if !ok {
		return false
	}
	if n.hasUnstableOffset || o.hasUnstableOffset {
		return false
	}
	return n.Range == o.Range &&
		n.Offset == o.Offset &&
		n.Step == o.Step &&
		equalInt64Ptr(n.Timestamp, o.Timestamp)
}

// Describe returns a short summary of this node for debugging.
func (n *SubqueryExprNode) Describe() string {
	return fmt.Sprintf("SubqueryExpr(range=%s, offset=%s, step=%s, timestamp=%s)",
		n.Range, n.Offset, n.Step, formatInt64Ptr(n.Timestamp))
}

// NumberLiteralNode represents a scalar number literal, mirroring
// parser.NumberLiteral.
type NumberLiteralNode struct {
	baseNode

	// Val is the literal's value.
	Val float64
}

// ChildCount always returns 0: a number literal is a leaf node.
func (n *NumberLiteralNode) ChildCount() int { return 0 }

// EquivalentTo reports whether other is a NumberLiteralNode with the same
// value as n. Two NaN values are considered equivalent to each other, since
// PromQL's own NaN is a single sentinel value here, not floating-point NaN
// comparison semantics.
func (n *NumberLiteralNode) EquivalentTo(other Node) bool {
	o, ok := other.(*NumberLiteralNode)
	if !ok {
		return false
	}
	if math.IsNaN(n.Val) && math.IsNaN(o.Val) {
		return true
	}
	return n.Val == o.Val
}

// Describe returns a short summary of this node for debugging.
func (n *NumberLiteralNode) Describe() string {
	return fmt.Sprintf("NumberLiteral(val=%g)", n.Val)
}

// StringLiteralNode represents a string literal, mirroring
// parser.StringLiteral.
type StringLiteralNode struct {
	baseNode

	// Val is the literal's value.
	Val string
}

// ChildCount always returns 0: a string literal is a leaf node.
func (n *StringLiteralNode) ChildCount() int { return 0 }

// EquivalentTo reports whether other is a StringLiteralNode with the same
// value as n.
func (n *StringLiteralNode) EquivalentTo(other Node) bool {
	o, ok := other.(*StringLiteralNode)
	if !ok {
		return false
	}
	return n.Val == o.Val
}

// Describe returns a short summary of this node for debugging.
func (n *StringLiteralNode) Describe() string {
	return fmt.Sprintf("StringLiteral(val=%q)", n.Val)
}

// UnaryExprNode represents a unary operation on its single child, mirroring
// parser.UnaryExpr.
type UnaryExprNode struct {
	baseNode

	// Op is the unary operation (currently only unary minus).
	Op parser.ItemType
}

// EquivalentTo reports whether other is a UnaryExprNode with the same
// operator as n.
func (n *UnaryExprNode) EquivalentTo(other Node) bool {
	o, ok := other.(*UnaryExprNode)
	if !ok {
		return false
	}
	return n.Op == o.Op
}

// Describe returns a short summary of this node for debugging.
func (n *UnaryExprNode) Describe() string {
	return fmt.Sprintf("UnaryExpr(op=%s)", n.Op)
}

// equalInt64Ptr reports whether a and b point to equal values, or are both nil.
func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// formatInt64Ptr formats a possibly-nil *int64 for use in Describe output.
func formatInt64Ptr(v *int64) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *v)
}

// equalMatchers reports whether a and b contain the same matchers in the
// same order. Order matters here because normalizing (sorting) matchers
// before comparison is a separate pass, NormalizeForCSE (see normalize.go
// and docs/query-planner-phase2-design.md §3), not something EquivalentTo
// does on every pairwise comparison.
func equalMatchers(a, b []*labels.Matcher) bool {
	if len(a) != len(b) {
		return false
	}
	for i, m := range a {
		o := b[i]
		if m.Type != o.Type || m.Name != o.Name || m.Value != o.Value {
			return false
		}
	}
	return true
}

// equalVectorMatching reports whether a and b describe the same vector
// matching behavior. Like equalMatchers, this compares MatchingLabels and
// Include in order; NormalizeForCSE (normalize.go) sorts both label lists
// once, up front, precisely so that this order-sensitive comparison still
// produces the semantically correct order-independent answer.
func equalVectorMatching(a, b *parser.VectorMatching) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Card == b.Card &&
		a.On == b.On &&
		slices.Equal(a.MatchingLabels, b.MatchingLabels) &&
		slices.Equal(a.Include, b.Include) &&
		equalFloat64Ptr(a.FillValues.RHS, b.FillValues.RHS) &&
		equalFloat64Ptr(a.FillValues.LHS, b.FillValues.LHS)
}

// equalFloat64Ptr reports whether a and b point to equal values, or are both nil.
func equalFloat64Ptr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
