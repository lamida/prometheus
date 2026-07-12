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
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// This file is execution-inert: nothing in this package consults engine.go,
// functions.go, or any other evaluation code, and this file does not add or
// remove either. It only computes whether a plan node's evaluation could
// legally produce two output series with the same labelset (as Prometheus's
// own engine defines "same labelset" via Series.ContainsSameLabelset /
// mergeSeriesWithSameLabelset, see promql/engine.go), and marks that fact on
// the plan graph via DeduplicateAndMergeNode. A future step is expected to
// consult that marker from engine.go to skip the ContainsSameLabelset check
// and the mergeSeriesWithSameLabelset merge pass at evaluation time for any
// subtree this package has proven unique — that wiring is out of scope here;
// this package only produces the proof.

// DeduplicateAndMergeNode marks a point in the plan graph whose single child
// may evaluate to a Vector or Matrix containing two or more series that
// share the same labelset (for example, because the child drops or rewrites
// the __name__ label, or is an `or` binary expression combining two
// independently-selected sides). DeduplicateAndMergeNode carries no fields
// of its own: it is a pure marker recording "a same-labelset check and merge
// is needed here," not a description of how to perform it — nothing about
// execution lives on this type, matching every other node in this package.
//
// EliminateDeduplicateAndMergeInsertion (called by FromExpr) is the only
// producer of this node type; EliminateDeduplicateAndMerge is the only
// consumer that removes it once it can prove the marker is unnecessary.
type DeduplicateAndMergeNode struct {
	baseNode
}

// EquivalentTo reports whether other is also a DeduplicateAndMergeNode.
// DeduplicateAndMergeNode has no type-specific fields, so any two
// DeduplicateAndMergeNodes are equivalent regardless of their children (see
// the Node.EquivalentTo doc for why children are never compared here).
func (n *DeduplicateAndMergeNode) EquivalentTo(other Node) bool {
	_, ok := other.(*DeduplicateAndMergeNode)
	return ok
}

// Describe returns a short summary of this node for debugging.
func (n *DeduplicateAndMergeNode) Describe() string {
	return "DeduplicateAndMerge"
}

// wrapInDeduplicateAndMerge wraps child in a new DeduplicateAndMergeNode
// when wrap is true, and returns child unchanged otherwise.
func wrapInDeduplicateAndMerge(child Node, wrap bool) Node {
	if !wrap {
		return child
	}
	n := &DeduplicateAndMergeNode{}
	n.SetChildren([]Node{child})
	return n
}

// retainsMetricNameForVectorScalarOp reports whether a vector-scalar binary
// operation with the given operator and `bool` modifier retains the vector
// operand's __name__ label, rather than dropping it.
//
// Verified directly against promql/engine.go's own vector-scalar evaluation,
// not assumed from Mimir's equivalent (promqlext.RetainsMetricName): see
// evaluator.VectorscalarBinop (engine.go, around line 3430), which sets
// DropName (and, when delayed name removal is disabled, actually strips
// __name__) whenever `changesMetricSchema(op) || returnBool`, and
// changesMetricSchema (engine.go, around line 4431) reports true for the
// arithmetic operators ADD, SUB, MUL, DIV, MOD, POW, and ATAN2. So __name__
// is retained only for: a comparison operator used without the `bool`
// modifier (a filter, which keeps the matching side's Metric untouched), or
// the trim operators TRIM_UPPER ("</") / TRIM_LOWER (">/"), neither of which
// appears in changesMetricSchema's list. This happens to match Mimir's
// RetainsMetricName exactly, but that was confirmed by reading engine.go
// directly rather than by assuming parity with Mimir.
func retainsMetricNameForVectorScalarOp(op parser.ItemType, returnBool bool) bool {
	return (op.IsComparisonOperator() && !returnBool) || op == parser.TRIM_UPPER || op == parser.TRIM_LOWER
}

// functionsThatNeverNeedDeduplication lists Vector-returning PromQL
// functions verified not to introduce duplicate output labelsets, so
// FromExpr never wraps a call to one of them in a DeduplicateAndMergeNode.
// Every function not listed here defaults to needing a wrap (see
// callNeedsDeduplicationWrap) — the same conservative default Mimir's own
// functionNeedsDeduplication switch uses for anything it can't classify.
//
// Scalar-returning functions (scalar, time, pi, max_of, min_of, and the
// range/start/end/step selector-range accessors) are not listed here at all:
// they are excluded earlier, by checking the call's own result type, since a
// Scalar result can never contain multiple series to begin with.
var functionsThatNeverNeedDeduplication = map[string]bool{
	// Verified: funcAbsent (functions.go) always returns at most one
	// synthetic series, built from the selector's own matchers, regardless
	// of how many series matched (or didn't). A single series can't collide
	// with itself.
	"absent": true,
	// Same reasoning as absent: funcAbsentOverTime (functions.go) also
	// always returns at most one synthetic series. Note this overrides the
	// generic range-vector-function drop-name rule below, which would
	// otherwise mark it as dropping __name__ (it does, mechanically), but
	// dropping __name__ on a single guaranteed-unique series can't create a
	// collision.
	"absent_over_time": true,
	// Verified: promql/engine.go's generic range-vector Call evaluation path
	// (around line 2278) explicitly excludes these two by name from the
	// drop-name rule: "the last_over_time and first_over_time functions act
	// like offset; thus, they should keep the metric name."
	"first_over_time": true,
	"last_over_time":  true,
	// Not independently re-derived from Prometheus's own evaluator; carried
	// over from Mimir's functionNeedsDeduplication "do NOT need dedup" list
	// as-is, since info()'s series-uniqueness argument depends on join
	// semantics this pass does not attempt to re-verify from first
	// principles.
	"info": true,
	// Verified: funcSort / funcSortDesc / funcSortByLabel /
	// funcSortByLabelDesc (functions.go) only reorder the input Vector; none
	// of them construct a new Metric or otherwise touch series labels, so
	// they can't turn a unique input into a colliding output.
	"sort":               true,
	"sort_desc":          true,
	"sort_by_label":      true,
	"sort_by_label_desc": true,
	// Verified: funcVector (functions.go) always returns a single series
	// with Metric: labels.Labels{} — no labels at all, let alone a
	// colliding one.
	"vector": true,
}

// callNeedsDeduplicationWrap reports whether a call to the named
// Vector-returning function should be wrapped in a DeduplicateAndMergeNode.
// See functionsThatNeverNeedDeduplication's doc for the verified exceptions;
// everything else defaults to true.
//
// This default covers, among others, every range-vector function except
// last_over_time/first_over_time (verified directly: promql/engine.go's
// generic range-vector Call evaluation path unconditionally sets dropName
// for all of them, around line 2278) and abs/ceil/floor/exp/ln/log2/log10/
// sin/cos/tan/asin/acos/atan/sinh/cosh/tanh/asinh/acosh/atanh/rad/deg/sgn/
// clamp/clamp_max/clamp_min/round/histogram_avg/histogram_count/
// histogram_sum/histogram_stddev/histogram_stdvar/histogram_fraction/
// histogram_quantile/timestamp/day_of_month/day_of_week/day_of_year/
// days_in_month/hour/minute/month/year/label_replace/label_join (verified
// directly: each sets DropName: true in its own implementation in
// promql/functions.go, via simpleFloatFunc, simpleHistogramFunc, clamp, or
// dateWrapper). predict_linear and double_exponential_smoothing are
// range-vector functions and so are already covered by the generic rule
// above.
func callNeedsDeduplicationWrap(name string) bool {
	return !functionsThatNeverNeedDeduplication[name]
}

// labelModifyingFunctions lists PromQL functions whose output labelset is
// not a deterministic function of solely the input series' own labels: they
// set or rewrite a label using their own (non-series) arguments
// (label_replace, label_join), or fan a single input series out into
// several output series distinguished by a label they add
// (histogram_quantiles, the multi-quantile sibling of histogram_quantile,
// which adds a configurable quantile label per output series). Even a
// provably-unique input can produce duplicate output labelsets through one
// of these, so EliminateDeduplicateAndMerge never eliminates a wrap sitting
// above a call to one of them, regardless of what it concludes about the
// call's arguments.
//
// Mirrors Mimir's isLabelModifyingFunction. histogram_quantile (singular) is
// deliberately not included: verified directly in promql/functions.go that
// it only ever sets DropName: true, without adding or rewriting any label,
// so a unique input to it stays unique.
var labelModifyingFunctions = map[string]bool{
	"label_replace":       true,
	"label_join":          true,
	"histogram_quantiles": true,
}

// hasExactNameMatcher reports whether matchers contains an equality matcher
// on the __name__ label (labels.MetricName), which is Prometheus's own
// guarantee that every series it selects shares one specific metric name —
// and therefore, combined with the rest of each series' labels already
// being unique per Prometheus's storage model, that the selection itself
// can't contain two series with the same labelset.
func hasExactNameMatcher(matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		if m.Type == labels.MatchEqual && m.Name == labels.MetricName {
			return true
		}
	}
	return false
}

// planValueType reports the PromQL value type a plan Node's evaluation
// would have. It exists solely so EliminateDeduplicateAndMerge can classify
// a BinaryExprNode's operands as vector or scalar the same way FromExpr did
// at insertion time from the original parser.Expr's own Type() — the plan
// graph no longer has the parser.Expr to ask directly. It does not attempt
// to be a general-purpose type checker: MatrixSelectorNode and
// SubqueryExprNode report ValueTypeMatrix, which is never itself a valid
// operand type, only an intermediate shape that appears as a CallNode's own
// child.
func planValueType(n Node) parser.ValueType {
	switch n := n.(type) {
	case *NumberLiteralNode:
		return parser.ValueTypeScalar
	case *StringLiteralNode:
		return parser.ValueTypeString
	case *MatrixSelectorNode:
		return parser.ValueTypeMatrix
	case *SubqueryExprNode:
		return parser.ValueTypeMatrix
	case *CallNode:
		if n.Func != nil {
			return n.Func.ReturnType
		}
		return parser.ValueTypeVector
	case *BinaryExprNode:
		if planValueType(n.Child(0)) == parser.ValueTypeScalar && planValueType(n.Child(1)) == parser.ValueTypeScalar {
			return parser.ValueTypeScalar
		}
		return parser.ValueTypeVector
	case *UnaryExprNode:
		return planValueType(n.Child(0))
	case *DeduplicateAndMergeNode:
		return planValueType(n.Child(0))
	default:
		// VectorSelectorNode and AggregateExprNode always evaluate to a
		// Vector; this default also covers them.
		return parser.ValueTypeVector
	}
}

// EliminateDeduplicateAndMerge removes DeduplicateAndMergeNode wraps from
// root's subtree that it can prove are unnecessary — i.e. where the wrapped
// subtree's evaluation is already guaranteed not to produce two series with
// the same labelset, so the same-labelset check and merge the marker
// represents (see DeduplicateAndMergeNode's doc) would always be a no-op.
// It returns the (possibly new) root of the rewritten subtree and the
// number of DeduplicateAndMergeNode wraps it eliminated.
//
// This is a plan-level rewrite only; it never touches promql/engine.go,
// promql/functions.go, or any other evaluation code, and today nothing
// consults its result — see this file's top-of-file doc comment. It mirrors
// Mimir's EliminateDeduplicateAndMergeOptimizationPass, run at the same
// point Mimir runs it: on a plain tree, before any common-subexpression
// sharing exists, so there is no risk of eliminating a wrap that some other
// parent still needs (every node in the tree this pass walks has exactly
// one parent).
//
// delayedNameRemoval must match the same-named promql/engine.go evaluator
// option (EngineOpts.EnableDelayedNameRemoval) the resulting plan will
// eventually be evaluated under: the two modes differ in when __name__ is
// actually removed from a series' labelset (immediately at each operator,
// vs. deferred to the end of evaluation), which changes which intermediate
// wraps are provably safe to eliminate. See
// canEliminateDeduplicateAndMergeDelayedNameRemoval's doc for specifics.
func EliminateDeduplicateAndMerge(root Node, delayedNameRemoval bool) (Node, int) {
	eliminated := 0

	for i := 0; i < root.ChildCount(); i++ {
		child := root.Child(i)
		newChild, n := EliminateDeduplicateAndMerge(child, delayedNameRemoval)
		eliminated += n
		if newChild != child {
			root.ReplaceChild(child, newChild)
		}
	}

	dedup, ok := root.(*DeduplicateAndMergeNode)
	if !ok {
		return root, eliminated
	}

	inner := dedup.Child(0)
	var canEliminate bool
	if delayedNameRemoval {
		canEliminate = canEliminateDeduplicateAndMergeDelayedNameRemoval(inner)
	} else {
		canEliminate = canEliminateDeduplicateAndMerge(inner)
	}
	if !canEliminate {
		return root, eliminated
	}
	return inner, eliminated + 1
}

// canEliminateDeduplicateAndMerge reports whether node's evaluation is
// guaranteed not to produce two series with the same labelset, under
// promql/engine.go's non-delayed name removal behavior (__name__ is removed
// from a series' labelset immediately at the operator that drops it, not
// deferred). Mirrors Mimir's canEliminateDeduplicateAndMerge.
//
// Every recursive case below defaults to false (cannot prove uniqueness) for
// any node type not explicitly handled — including NumberLiteralNode and
// StringLiteralNode. This is a direct, deliberate port of a real (if
// initially surprising) conservatism already present in Mimir's own switch:
// a CallNode with even one scalar-literal argument alongside its Vector
// argument (e.g. `clamp(foo, 0, 1)`, `round(foo, 5)`) can never have its
// wrap eliminated, because the loop over the call's arguments below recurses
// into the literal argument too and that recursion always returns false.
// This is not fixed here since the goal is a faithful port of Mimir's
// behavior, not an improvement on it.
func canEliminateDeduplicateAndMerge(node Node) bool {
	switch n := node.(type) {
	case *VectorSelectorNode:
		return hasExactNameMatcher(n.LabelMatchers)
	case *MatrixSelectorNode:
		vs, ok := n.Child(0).(*VectorSelectorNode)
		if !ok {
			return false
		}
		return hasExactNameMatcher(vs.LabelMatchers)
	case *CallNode:
		if n.Func != nil && labelModifyingFunctions[n.Func.Name] {
			return false
		}
		for i := 0; i < n.ChildCount(); i++ {
			if !canEliminateDeduplicateAndMerge(n.Child(i)) {
				return false
			}
		}
		return true
	case *SubqueryExprNode:
		return canEliminateDeduplicateAndMerge(n.Child(0))
	case *UnaryExprNode:
		return canEliminateDeduplicateAndMerge(n.Child(0))
	case *DeduplicateAndMergeNode, *AggregateExprNode:
		return true
	case *BinaryExprNode:
		if n.Op == parser.LOR {
			return false
		}
		lhsType := planValueType(n.Child(0))
		rhsType := planValueType(n.Child(1))
		isVectorScalar := lhsType != rhsType && (lhsType == parser.ValueTypeScalar || rhsType == parser.ValueTypeScalar)
		return !isVectorScalar
	default:
		return false
	}
}

// canEliminateDeduplicateAndMergeDelayedNameRemoval reports whether node's
// evaluation is guaranteed not to produce two series with the same
// labelset, under promql/engine.go's delayed name removal behavior
// (EnableDelayedNameRemoval: __name__ removal is deferred; intermediate
// operators only set the DropName flag on a Series/Sample rather than
// stripping the label immediately, see e.g. engine.go's
// `!ev.enableDelayedNameRemoval && seriesDropName` guards). Mirrors Mimir's
// canEliminateDeduplicateAndMergeDelayedNameRemoval.
//
// This is deliberately more permissive than canEliminateDeduplicateAndMerge
// for two node types, matching an asymmetry present in Mimir's own two
// functions rather than one introduced here:
//   - BinaryExprNode only checks for `or`; a vector-scalar operation that
//     drops __name__ is not itself disqualifying, because under delayed
//     removal the label is still physically present (only flagged for
//     removal) at the point this operator runs, so it cannot yet collide
//     with anything — only the final removal, deferred to the end of
//     evaluation, could, and that is outside what any single intermediate
//     DeduplicateAndMergeNode wrap needs to account for.
//   - Every node type not explicitly handled below (including
//     VectorSelectorNode, MatrixSelectorNode, SubqueryExprNode,
//     UnaryExprNode, NumberLiteralNode, StringLiteralNode,
//     DeduplicateAndMergeNode, and AggregateExprNode) defaults to true, not
//     false as in the non-delayed variant, again mirroring Mimir's switch
//     verbatim rather than trying to smooth the asymmetry away.
//
// Mimir's variant recurses from a dedicated core.DropName plan node inserted
// only once, at the root of the whole query, by a separate pass
// (insertDropNameOperator) that this package deliberately does not port:
// Prometheus has no DropName plan node at all (see this file's top-of-file
// doc comment and FromExpr's doc comment on how DropName is represented),
// so there is nothing structurally analogous to recurse from at the root.
// This function is applied at every DeduplicateAndMergeNode in the tree
// instead — the same place the non-delayed variant is applied — which is a
// simplification relative to Mimir's root-only insertion, not a rediscovery
// of new behavior.
func canEliminateDeduplicateAndMergeDelayedNameRemoval(node Node) bool {
	switch n := node.(type) {
	case *BinaryExprNode:
		return n.Op != parser.LOR
	case *CallNode:
		if n.Func != nil && labelModifyingFunctions[n.Func.Name] {
			return false
		}
		for i := 0; i < n.ChildCount(); i++ {
			if !canEliminateDeduplicateAndMergeDelayedNameRemoval(n.Child(i)) {
				return false
			}
		}
		return true
	default:
		return true
	}
}
