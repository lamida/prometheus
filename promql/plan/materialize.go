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

import "github.com/prometheus/prometheus/promql/parser"

// This file is where the plan graph's sharing decisions (introduced by
// CommonSubexpressionElimination) get projected back onto the real
// parser.Expr tree that promql/engine.go actually walks. Everything else in
// this package (build.go, cse.go, normalize.go, dedup.go) only ever
// mutates/reads the plan graph; MaterializeSharing is the one function that
// mutates the original parser.Expr tree FromExpr was built from.

// occurrenceRecord describes one real parser.Expr occurrence a plan Node
// was built from: the concrete parser.Expr value itself, how to overwrite
// the field that occurrence was read from (so a later occurrence of an
// equivalent node can be aliased onto this one, or vice versa), and whether
// this particular occurrence is eligible to be aliased at all.
//
// A node built directly by FromExpr always has exactly one occurrenceRecord
// (FromExpr never introduces sharing). When CommonSubexpressionElimination
// merges two equivalent nodes onto one survivor, the survivor accumulates
// every merged node's occurrenceRecord (see cse.go's canonicalize and
// baseNode.addOccurrences), so no occurrence is ever silently dropped by
// merging.
type occurrenceRecord struct {
	// realExpr is the concrete parser.Expr node this occurrence's plan Node
	// was built from — the same (unwrapped) value fromExpr's type switch
	// dispatched on.
	realExpr parser.Expr
	// setRealExpr, if non-nil, overwrites the field this occurrence's
	// parent read realExpr from with a new parser.Expr value. It is nil for
	// the plan's root occurrence (no parent field exists to rewrite) and
	// for any occurrence immediately unwrapped from a
	// *parser.StepInvariantExpr, which is always ineligibleForSharing
	// regardless (see fromExpr's doc) — nil makes it impossible to
	// accidentally materialize into that slot.
	setRealExpr func(parser.Expr)
	// ineligibleForSharing is true when this occurrence must never be
	// materialized/aliased with another occurrence: it sits underneath a
	// *parser.SubqueryExpr, or underneath a *parser.StepInvariantExpr,
	// anywhere in its ancestry. Both cases evaluate this occurrence's whole
	// subtree using a separately-scoped, freshly constructed *evaluator
	// (see promql/engine.go's evalSubquery/runSubquery and the
	// *parser.StepInvariantExpr case in eval()), so aliasing this
	// occurrence's real parser.Expr pointer with one evaluated by the
	// outer, top-level *evaluator would let the outer evaluator's
	// sharedNodeRefcount bookkeeping count a consumer that will never
	// actually visit it (the nested evaluator never consults the outer
	// evaluator's cache), permanently stalling that node's refcount and
	// leaking its pooled point slices for the lifetime of the query. See
	// fromExpr's doc for why this is tracked transitively for both
	// boundaries, not just the immediate wrap point.
	ineligibleForSharing bool
}

// newOccurrenceRecord builds the occurrenceRecord for a node built from
// realExpr, with setter setRealExpr, given the insideSubquery/
// insideStepInvariant flags in effect at the moment realExpr's plan Node
// was constructed.
func newOccurrenceRecord(realExpr parser.Expr, setRealExpr func(parser.Expr), insideSubquery, insideStepInvariant bool) occurrenceRecord {
	return occurrenceRecord{
		realExpr:             realExpr,
		setRealExpr:          setRealExpr,
		ineligibleForSharing: insideSubquery || insideStepInvariant,
	}
}

// occurrenceHolder is implemented by every Node built by this package (via
// baseNode): it exposes the occurrence-tracking baseNode itself owns, so
// cse.go and this file can read/merge it without a per-concrete-type
// switch.
type occurrenceHolder interface {
	addOccurrence(occurrenceRecord)
	addOccurrences([]occurrenceRecord)
	occurrenceRecords() []occurrenceRecord
}

// StripDeduplicateAndMergeMarkers removes every DeduplicateAndMergeNode
// from root's subtree unconditionally, replacing each with its single
// child. Unlike EliminateDeduplicateAndMerge (dedup.go), which only removes
// a marker it can prove is semantically unnecessary, this removes every
// marker regardless: DeduplicateAndMergeNode has no corresponding
// parser.Expr type at all (see dedup.go's package doc), so it is not
// something MaterializeSharing — or NormalizeForCSE, or
// CommonSubexpressionElimination — has any way to reason about. Call this
// once, immediately after FromExpr and before NormalizeForCSE, so every
// later pass in this package only ever sees node types with a real
// parser.Expr counterpart.
//
// It returns the (possibly new) root of the rewritten subtree.
func StripDeduplicateAndMergeMarkers(root Node) Node {
	if root == nil {
		return nil
	}

	for i := 0; i < root.ChildCount(); i++ {
		child := root.Child(i)
		newChild := StripDeduplicateAndMergeMarkers(child)
		if newChild != child {
			root.ReplaceChild(child, newChild)
		}
	}

	if dedup, ok := root.(*DeduplicateAndMergeNode); ok {
		return dedup.Child(0)
	}
	return root
}

// MaterializeSharing walks root's plan graph (which must already have had
// StripDeduplicateAndMergeMarkers, NormalizeForCSE, and
// CommonSubexpressionElimination run over it, in that order) and projects
// every merged node's sharing decision back onto the real parser.Expr tree:
// for each plan Node with two or more occurrenceRecords not marked
// ineligibleForSharing, one such occurrence's realExpr is chosen as
// canonical, and every other eligible occurrence's setRealExpr is invoked
// with that canonical value — literally repointing the corresponding
// parent's field in the real parser.Expr tree at the same object.
//
// It returns a map from each canonical parser.Expr that got materialized
// this way to the number of eligible occurrences that now alias it (always
// >= 2). promql/engine.go uses this as the refcount table it needs to know
// how many times a shared node's result will be consulted, and how many
// times its underlying pooled point slices must be visited, before it is
// safe to release them back to the pool.
//
// A plan Node whose ParentCount() is greater than 1 but that has fewer than
// two ELIGIBLE occurrences (e.g. every occurrence but one sits under a
// subquery or step-invariant boundary) is left untouched: nothing is
// materialized for it, and it is not a key in the returned map.
func MaterializeSharing(root Node) map[parser.Expr]int {
	refcounts := make(map[parser.Expr]int)
	visited := make(map[Node]bool)
	materializeWalk(root, visited, refcounts)
	return refcounts
}

// materializeWalk visits every node reachable from n exactly once (guarded
// by visited, since a node with ParentCount() > 1 is reachable via more
// than one path), materializing each node's occurrence sharing as it goes.
func materializeWalk(n Node, visited map[Node]bool, refcounts map[parser.Expr]int) {
	if n == nil || visited[n] {
		return
	}
	visited[n] = true

	for i := 0; i < n.ChildCount(); i++ {
		materializeWalk(n.Child(i), visited, refcounts)
	}

	holder, ok := n.(occurrenceHolder)
	if !ok {
		return
	}
	occurrences := holder.occurrenceRecords()

	eligible := make([]occurrenceRecord, 0, len(occurrences))
	for _, o := range occurrences {
		if !o.ineligibleForSharing {
			eligible = append(eligible, o)
		}
	}
	if len(eligible) < 2 {
		return
	}

	canonical := eligible[0].realExpr
	for _, o := range eligible[1:] {
		if o.setRealExpr != nil && o.realExpr != canonical {
			o.setRealExpr(canonical)
		}
	}
	refcounts[canonical] = len(eligible)
}
