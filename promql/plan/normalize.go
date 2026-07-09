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
	"cmp"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

// NormalizeForCSE sorts label lists on root's subtree that are semantically
// sets, not sequences, so that two structurally-identical subtrees differing
// only in the textual order of such a list compare equal under
// EquivalentTo. It must be run once, before CommonSubexpressionElimination,
// rather than folded into EquivalentTo itself, which would re-sort on every
// pairwise comparison instead of once up front (see
// docs/query-planner-phase2-design.md §3's closing paragraph).
//
// NormalizeForCSE mutates the relevant fields in place; it never changes
// the tree's shape (no SetChildren/ReplaceChild calls), so it is safe to
// call directly on a tree that already has parents pointing at it.
//
// Three fields are normalized, each verified order-independent against how
// promql/engine.go actually uses it before being included here:
//
//   - VectorSelectorNode.LabelMatchers, sorted by (Name, Type, Value).
//     Matching a series against a set of matchers doesn't depend on the
//     matchers' order: every matcher must independently match, and none of
//     them observe each other.
//   - BinaryExprNode.VectorMatching's MatchingLabels and Include. Verified
//     against labels.Labels.MatchLabels (model/labels/labels_*.go), which
//     builds a name set from MatchingLabels before testing series labels
//     against it, and against labels.Builder.Keep/Del (used for Include,
//     model/labels/labels_common.go), each of which treats its ns ...string
//     argument as an independent per-name operation — order does not change
//     the result of either.
//   - AggregateExprNode.Grouping. Verified against promql/engine.go's own
//     *parser.AggregateExpr evaluation (around engine.go:2096): "Grouping
//     labels must be sorted (expected both by generateGroupingKey() and
//     aggregation())" — engine.go sorts Grouping itself before using it, so
//     this plan-level sort is not introducing any new order-independence
//     assumption, only doing early what the evaluator already does anyway.
//
// parser.VectorMatching.FillValues (RHS/LHS group_left/group_right defaults)
// is deliberately left untouched: it is a pair of at-most-one values, not a
// list, so there is nothing to sort.
func NormalizeForCSE(root Node) {
	normalizeNode(root, make(map[Node]bool))
}

// normalizeNode walks root's subtree, normalizing each node once. visited
// guards against revisiting a node that already has more than one parent
// (possible if NormalizeForCSE is ever run on a tree that already has
// sharing, e.g. a second pass over previously-merged output) so that a
// shared node's fields aren't sorted redundantly.
func normalizeNode(n Node, visited map[Node]bool) {
	if n == nil || visited[n] {
		return
	}
	visited[n] = true

	switch t := n.(type) {
	case *VectorSelectorNode:
		sortMatchers(t.LabelMatchers)
	case *BinaryExprNode:
		if t.VectorMatching != nil {
			slices.Sort(t.VectorMatching.MatchingLabels)
			slices.Sort(t.VectorMatching.Include)
		}
	case *AggregateExprNode:
		slices.Sort(t.Grouping)
	}

	for i := 0; i < n.ChildCount(); i++ {
		normalizeNode(n.Child(i), visited)
	}
}

// sortMatchers sorts matchers in place by (Name, Type, Value), a stable,
// arbitrary-but-deterministic key: what matters for EquivalentTo is only
// that two structurally-identical matcher sets, in whatever original order,
// always sort to the same sequence, not what that sequence actually is.
func sortMatchers(matchers []*labels.Matcher) {
	slices.SortFunc(matchers, func(a, b *labels.Matcher) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})
}
