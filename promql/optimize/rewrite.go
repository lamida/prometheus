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

package optimize

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"
)

// TransformChildren calls fn on each direct child expression of node and
// writes the result back into node's corresponding field or slice element.
//
// parser.Walk and parser.Inspect only ever hand a visitor the node itself,
// so a visitor can mutate fields *on* that node but has no way to replace
// the node in its parent's slot. TransformChildren fills that gap: it knows,
// for every concrete parser.Expr type the parser produces, which fields hold
// child expressions, and assigns fn's return value back into them. This is
// the same per-node-type switch PreprocessExpr's preprocessExprHelper
// hand-rolls for its own rewrites (promql/engine.go), factored out so
// optimization passes in this package don't each reimplement it.
//
// TransformChildren does not recurse; callers that need a full top-down or
// bottom-up rewrite should call TransformChildren from within their own
// recursive function, transforming a node's children before or after
// visiting the node itself as their pass requires.
//
// fn must not be nil. If fn returns an error, TransformChildren stops and
// returns that error immediately, leaving any already-rewritten children in
// place.
//
// node's children are limited to the shape the parser itself produces, plus
// parser.StepInvariantExpr's single child, matching parser.ChildrenIter.
// DurationExpr's own LHS/RHS operands are not visited: they are duration
// arithmetic, resolved separately by PreprocessExpr's duration visitor, not
// part of the value-expression tree these passes rewrite.
func TransformChildren(node parser.Expr, fn func(parser.Expr) (parser.Expr, error)) error {
	switch n := node.(type) {
	case *parser.AggregateExpr:
		if n.Expr != nil {
			expr, err := fn(n.Expr)
			if err != nil {
				return err
			}
			n.Expr = expr
		}
		if n.Param != nil {
			param, err := fn(n.Param)
			if err != nil {
				return err
			}
			n.Param = param
		}
	case *parser.BinaryExpr:
		lhs, err := fn(n.LHS)
		if err != nil {
			return err
		}
		n.LHS = lhs

		rhs, err := fn(n.RHS)
		if err != nil {
			return err
		}
		n.RHS = rhs
	case *parser.Call:
		for i, arg := range n.Args {
			rewritten, err := fn(arg)
			if err != nil {
				return err
			}
			n.Args[i] = rewritten
		}
	case *parser.SubqueryExpr:
		expr, err := fn(n.Expr)
		if err != nil {
			return err
		}
		n.Expr = expr
	case *parser.ParenExpr:
		expr, err := fn(n.Expr)
		if err != nil {
			return err
		}
		n.Expr = expr
	case *parser.UnaryExpr:
		expr, err := fn(n.Expr)
		if err != nil {
			return err
		}
		n.Expr = expr
	case *parser.MatrixSelector:
		expr, err := fn(n.VectorSelector)
		if err != nil {
			return err
		}
		n.VectorSelector = expr
	case *parser.StepInvariantExpr:
		expr, err := fn(n.Expr)
		if err != nil {
			return err
		}
		n.Expr = expr
	case *parser.NumberLiteral, *parser.StringLiteral, *parser.VectorSelector:
		// Leaf nodes: nothing to transform.
	default:
		return fmt.Errorf("optimize: TransformChildren: unhandled node type %T", node)
	}
	return nil
}
