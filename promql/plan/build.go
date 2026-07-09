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
		return n, nil

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
		return n, nil

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
		return n, nil

	default:
		return nil, fmt.Errorf("plan: FromExpr: unhandled node type %T", expr)
	}
}
