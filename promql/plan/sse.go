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

import "github.com/prometheus/prometheus/model/labels"

// This file implements Subset Selector Elimination (SSE) detection, the
// relation docs/query-planner-phase4-design.md proposes: node A subsumes
// node B if, for every possible evaluation time, B's result set is
// guaranteed to be a subset of A's. Unlike CommonSubexpressionElimination's
// EquivalentTo (an equivalence relation, merged via one bottom-up bucketing
// pass — see cse.go), subsumption is a partial order, so this is a
// dedicated pass, not a CSE bucket variant (§2).
//
// v1 scope, matching the design doc exactly: VectorSelectorNode only (no
// MatrixSelectorNode/range selectors — Open Question 3, resolved narrow for
// the same reason CSE itself started narrow), exact-matcher-superset
// containment only (no regex containment reasoning), and the same
// hasUnstableOffset conservatism CSE's own EquivalentTo already applies
// (Open Question 4, resolved: identical guard, since it is the exact same
// setOffsetForAtModifier hazard either relation would otherwise expose a
// query to).
//
// This does not check parser.VectorSelector's Smoothed/Anchored fields,
// because plan.FromExpr does not capture them on VectorSelectorNode at all
// today — CommonSubexpressionElimination's own EquivalentTo has the same
// gap. Fixing that is a pre-existing CSE concern, out of scope for this
// pass to take on alone; SSE is no less safe than CSE already is here, not
// more.

// SubsetSelectorElimination finds, among root's VectorSelectorNodes, pairs
// where one node's LabelMatchers are a strict matcher-superset of another's,
// and records each subsumed node's cheapest available source (the eligible
// subsuming candidate with the fewest matchers, ties broken by Describe()
// for determinism) in its subsetSource field. It returns the number of
// subsumption relations found.
//
// Must run after CommonSubexpressionElimination on the same root: SSE only
// needs to consider the already-deduplicated selector set (§2) — running it
// first would waste work comparing nodes CSE is about to merge anyway, and
// would leave subsetSource pointing at a node CSE then discards.
//
// A VectorSelectorNode reachable only as a MatrixSelectorNode's child is
// excluded from candidacy, both as a subsumed node and as a source: a
// MatrixSelectorNode's child is never evaluated through the code path
// SubsetSelectorElimination's execution wiring (promql/engine.go's
// evalUncached *parser.VectorSelector case) hooks into — see
// docs/query-planner-phase4-design.md's execution-wiring notes — so a
// relation involving one would simply never fire, while still (harmlessly
// but wastefully) inflating the source's expected-consumer count. Excluding
// it here is simpler than tracking per-occurrence eligibility for this
// narrow case.
func SubsetSelectorElimination(root Node) int {
	all := make(map[*VectorSelectorNode]bool)
	matrixNested := make(map[*VectorSelectorNode]bool)
	visited := make(map[Node]bool)

	var walk func(Node)
	walk = func(n Node) {
		if n == nil || visited[n] {
			return
		}
		visited[n] = true

		if m, ok := n.(*MatrixSelectorNode); ok {
			if vs, ok := m.Child(0).(*VectorSelectorNode); ok {
				matrixNested[vs] = true
			}
		}
		if vs, ok := n.(*VectorSelectorNode); ok {
			all[vs] = true
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)

	candidates := make([]*VectorSelectorNode, 0, len(all))
	for vs := range all {
		if matrixNested[vs] {
			continue
		}
		candidates = append(candidates, vs)
	}

	found := 0
	for _, b := range candidates {
		var best *VectorSelectorNode
		for _, a := range candidates {
			if a == b || !subsumes(a, b) {
				continue
			}
			if best == nil ||
				len(a.LabelMatchers) < len(best.LabelMatchers) ||
				(len(a.LabelMatchers) == len(best.LabelMatchers) && a.Describe() < best.Describe()) {
				best = a
			}
		}
		if best != nil {
			b.subsetSource = best
			found++
		}
	}
	return found
}

// subsumes reports whether a subsumes b: same Name/Offset/Timestamp/
// SkipHistogramBuckets as EquivalentTo requires, neither has an unstable
// offset (see hasUnstableOffset's doc), and a's LabelMatchers are a strict
// matcher-subset of b's (a has fewer matchers, and every one of a's
// matchers is also present, by exact value, in b's).
func subsumes(a, b *VectorSelectorNode) bool {
	if a.hasUnstableOffset || b.hasUnstableOffset {
		return false
	}
	if a.Name != b.Name ||
		a.Offset != b.Offset ||
		a.SkipHistogramBuckets != b.SkipHistogramBuckets ||
		!equalInt64Ptr(a.Timestamp, b.Timestamp) {
		return false
	}
	if len(a.LabelMatchers) >= len(b.LabelMatchers) {
		return false
	}
	return isMatcherSubset(a.LabelMatchers, b.LabelMatchers)
}

// isMatcherSubset reports whether every matcher in a is also present, by
// exact (Type, Name, Value) value, in b. Matcher type is compared by value,
// never interpreted (see docs/query-planner-phase4-design.md §1): this
// never attempts regex containment reasoning.
func isMatcherSubset(a, b []*labels.Matcher) bool {
	for _, ma := range a {
		found := false
		for _, mb := range b {
			if ma.Type == mb.Type && ma.Name == mb.Name && ma.Value == mb.Value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
