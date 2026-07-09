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

	"github.com/prometheus/prometheus/promql/parser"
)

// FromExpr converts expr into a QueryPlan: one plan Node per parser.Expr
// node, in a straightforward recursive walk. No sharing is introduced —
// the result is always a tree, never a DAG; sharing is a later pass's job
// (see docs/query-planner-phase2-design.md §2-§3), not this function's.
//
// expr must already have been run through parser.PreprocessExpr for the
// same start/end/step the caller intends to evaluate over (see
// promql/engine.go's PreprocessExpr). FromExpr relies on durations, step
// invariance, and offset/timestamp resolution having already happened: it
// reads the already-resolved Offset/Timestamp/Range fields directly off
// each parser.Expr node rather than recomputing them, and it drops
// parser.ParenExpr and parser.StepInvariantExpr wrappers entirely rather
// than modeling them as plan nodes.
//
// parser.ParenExpr carries no information beyond operator-precedence
// grouping, which the plan graph's explicit parent/child edges already make
// unambiguous, so it is unwrapped: FromExpr recurses directly into its
// single child. parser.StepInvariantExpr is the synthetic wrapper
// PreprocessExpr itself introduces to record that a subexpression evaluates
// to the same result at every step; that fact is a decision about how to
// evaluate the wrapped expression, not part of its shape, so it is likewise
// unwrapped by recursing into its single child. Nothing on
// StepInvariantExpr itself needs to survive onto the child's plan node:
// its only field is the wrapped Expr, so there is nothing to lose.
//
// Every node returned by FromExpr starts with ParentCount() == 0: FromExpr
// never introduces sharing, and it never calls SetChildren on a synthetic
// "parent" of its own return value, so nothing yet points at it. A caller
// that hangs the result off some other node (e.g. a future rewrite pass)
// is responsible for that node's own SetChildren call updating this node's
// ParentCount as usual.
//
// FromExpr returns an error, rather than panicking, for any parser.Expr
// type it does not explicitly handle — including parser.DurationExpr and
// parser.EvalStmt, which PreprocessExpr never leaves in the value-expression
// tree FromExpr is meant to walk.
//
// FromExpr also inserts DeduplicateAndMergeNode wraps around any BinaryExpr,
// Call, or UnaryExpr node whose evaluation could produce two series sharing
// the same labelset (see DeduplicateAndMergeNode's doc, and
// binaryExprNeedsDeduplicateAndMerge/callExprNeedsDeduplicateAndMerge's docs
// in dedup.go for exactly which ones and why), mirroring the wrapping Mimir's
// own nodeFromExpr performs — ordered, as docs/query-planner-phase2-design.md
// §6 calls for, before any common-subexpression-elimination pass exists to
// introduce sharing. This insertion happens inline in fromExpr below, rather
// than as a separate pass run immediately after it, because every wrap
// decision needs details only available on the original parser.Expr node at
// the moment its plan Node is built (e.g. *parser.BinaryExpr.LHS.Type(),
// which a later pass would have to re-derive from the plan graph instead;
// see dedup.go's planValueType for that more awkward re-derivation, needed
// only by the elimination side, which by contrast has no parser.Expr to
// consult).
func FromExpr(expr parser.Expr) (*QueryPlan, error) {
	root, err := fromExpr(expr)
	if err != nil {
		return nil, err
	}
	return &QueryPlan{Root: root}, nil
}

func fromExpr(expr parser.Expr) (Node, error) {
	switch e := expr.(type) {
	case *parser.ParenExpr:
		return fromExpr(e.Expr)
	case *parser.StepInvariantExpr:
		return fromExpr(e.Expr)

	case *parser.VectorSelector:
		return &VectorSelectorNode{
			Name:                 e.Name,
			LabelMatchers:        e.LabelMatchers,
			Offset:               e.Offset,
			Timestamp:            e.Timestamp,
			SkipHistogramBuckets: e.SkipHistogramBuckets,
		}, nil

	case *parser.MatrixSelector:
		child, err := fromExpr(e.VectorSelector)
		if err != nil {
			return nil, err
		}
		n := &MatrixSelectorNode{Range: e.Range}
		n.SetChildren([]Node{child})
		return n, nil

	case *parser.BinaryExpr:
		lhs, err := fromExpr(e.LHS)
		if err != nil {
			return nil, err
		}
		rhs, err := fromExpr(e.RHS)
		if err != nil {
			return nil, err
		}
		n := &BinaryExprNode{
			Op:             e.Op,
			VectorMatching: e.VectorMatching,
			ReturnBool:     e.ReturnBool,
		}
		n.SetChildren([]Node{lhs, rhs})
		return wrapInDeduplicateAndMerge(n, binaryExprNeedsDeduplicateAndMerge(e)), nil

	case *parser.AggregateExpr:
		child, err := fromExpr(e.Expr)
		if err != nil {
			return nil, err
		}
		n := &AggregateExprNode{
			Op:       e.Op,
			Grouping: e.Grouping,
			Without:  e.Without,
			HasParam: e.Param != nil,
		}
		children := []Node{child}
		if e.Param != nil {
			param, err := fromExpr(e.Param)
			if err != nil {
				return nil, err
			}
			children = append(children, param)
		}
		n.SetChildren(children)
		return n, nil

	case *parser.Call:
		children := make([]Node, 0, len(e.Args))
		for _, arg := range e.Args {
			child, err := fromExpr(arg)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		n := &CallNode{Func: e.Func}
		n.SetChildren(children)
		return wrapInDeduplicateAndMerge(n, callExprNeedsDeduplicateAndMerge(e)), nil

	case *parser.SubqueryExpr:
		child, err := fromExpr(e.Expr)
		if err != nil {
			return nil, err
		}
		n := &SubqueryExprNode{
			Range:     e.Range,
			Offset:    e.Offset,
			Timestamp: e.Timestamp,
			Step:      e.Step,
		}
		n.SetChildren([]Node{child})
		return n, nil

	case *parser.NumberLiteral:
		return &NumberLiteralNode{Val: e.Val}, nil

	case *parser.StringLiteral:
		return &StringLiteralNode{Val: e.Val}, nil

	case *parser.UnaryExpr:
		child, err := fromExpr(e.Expr)
		if err != nil {
			return nil, err
		}
		n := &UnaryExprNode{Op: e.Op}
		n.SetChildren([]Node{child})
		// Unary negation of a vector drops the __name__ label (verified
		// directly against promql/engine.go, around line 2487-2492: the
		// *parser.UnaryExpr/parser.SUB evaluation path unconditionally sets
		// DropName on every series it negates), so wrap in
		// DeduplicateAndMergeNode. Unary plus (parser.ADD) and unary minus
		// of a scalar never reach here needing a wrap: neither touches
		// __name__.
		needsWrap := e.Op == parser.SUB && e.Expr.Type() == parser.ValueTypeVector
		return wrapInDeduplicateAndMerge(n, needsWrap), nil

	default:
		return nil, fmt.Errorf("plan: FromExpr: unhandled node type %T", expr)
	}
}

// binaryExprNeedsDeduplicateAndMerge reports whether e's BinaryExprNode
// should be wrapped in a DeduplicateAndMergeNode. Two cases can produce
// colliding output labelsets:
//
//   - e.Op == parser.LOR: `or` independently selects from both sides and
//     concatenates them (see promql/engine.go's `or` handling), so nothing
//     prevents the same labelset appearing on both sides.
//   - e is a vector-scalar operation (exactly one side is
//     parser.ValueTypeScalar) that drops the vector side's __name__ label,
//     per retainsMetricNameForVectorScalarOp. Vector-vector operations are
//     not wrapped here at all (other than `or`, above): the operator's own
//     one-to-one/many-to-one series matching is trusted not to introduce a
//     collision by itself, mirroring Mimir's own nodeFromExpr, which only
//     ever wraps `or` and non-retaining vector-scalar binary expressions.
func binaryExprNeedsDeduplicateAndMerge(e *parser.BinaryExpr) bool {
	if e.Op == parser.LOR {
		return true
	}

	lhsType := e.LHS.Type()
	rhsType := e.RHS.Type()
	isVectorScalar := lhsType != rhsType && (lhsType == parser.ValueTypeScalar || rhsType == parser.ValueTypeScalar)
	if !isVectorScalar {
		return false
	}
	return !retainsMetricNameForVectorScalarOp(e.Op, e.ReturnBool)
}

// callExprNeedsDeduplicateAndMerge reports whether e's CallNode should be
// wrapped in a DeduplicateAndMergeNode. A call that doesn't return a Vector
// (e.g. scalar(), time()) is never wrapped: a Scalar result can't contain
// more than one value, let alone two colliding ones. Otherwise, see
// callNeedsDeduplicationWrap.
func callExprNeedsDeduplicateAndMerge(e *parser.Call) bool {
	if e.Func == nil || e.Func.ReturnType != parser.ValueTypeVector {
		return false
	}
	return callNeedsDeduplicationWrap(e.Func.Name)
}
