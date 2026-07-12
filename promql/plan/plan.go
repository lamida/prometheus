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

// Package plan defines a DAG-capable query plan graph for PromQL, built from
// an already-preprocessed parser.Expr tree by FromExpr.
//
// Unlike a parser.Expr tree, a plan Node may have more than one parent: a
// later pass (common subexpression elimination, not part of this package
// yet) introduces sharing by pointing multiple parents at the same Node.
// This package only builds the plan graph; it does not consult or execute
// it. See docs/query-planner-phase2-design.md §1-§2 for the design this
// package implements.
package plan

import "fmt"

// Node is a node in the query plan graph. Unlike a parser.Expr tree, a Node
// may have more than one parent: a future common-subexpression-elimination
// pass introduces sharing by pointing multiple parents at the same Node.
//
// Node is purely structural: it describes shape (children) and equivalence,
// never how a node executes. That is deliberate — see
// docs/query-planner-phase2-design.md §1 for why execution stays out of this
// interface.
type Node interface {
	// ChildCount returns the number of direct children of this node.
	ChildCount() int
	// Child returns the direct child at idx, which must be in
	// [0, ChildCount()). Child panics if idx is out of range.
	Child(idx int) Node
	// SetChildren replaces this node's entire list of children with
	// children, adjusting the ParentCount of every removed and added child
	// accordingly. Passing the same Node more than once in children is
	// legal: that Node's ParentCount is incremented once per occurrence,
	// matching how many of this node's child slots reference it.
	SetChildren(children []Node)
	// ReplaceChild replaces the first occurrence of oldChild with newChild
	// among this node's children, decrementing oldChild's ParentCount and
	// incrementing newChild's ParentCount accordingly. If oldChild appears
	// more than once among this node's children, only the first occurrence
	// is replaced. ReplaceChild panics if oldChild is not a child of this
	// node. Replacing a child with itself is a no-op: ParentCount is left
	// unchanged.
	ReplaceChild(oldChild, newChild Node)

	// ParentCount reports how many child slots, across every node in the
	// plan graph, currently reference this Node. This is explicit, tracked
	// bookkeeping rather than something derived by walking the graph: a
	// future execution strategy for shared nodes needs to know exactly when
	// the last consumer of a shared node has read its result, and
	// re-deriving that by re-walking the whole plan from the root on every
	// visit would be needlessly expensive. Nodes built by FromExpr start at
	// ParentCount() == 0, since FromExpr never introduces sharing and
	// nothing points at a freshly built node's children until SetChildren
	// or ReplaceChild is called on their parent.
	ParentCount() int

	// EquivalentTo reports whether this node and other are structurally
	// interchangeable: same concrete node type and same type-specific
	// fields (operation, matchers, range, offset, timestamp, and so on).
	// EquivalentTo never inspects children — comparing a node's subtree for
	// equivalence is a separate, path-dependent concern (see
	// docs/query-planner-phase2-design.md §3) left to a future
	// common-subexpression-elimination pass, which is the actual consumer
	// of this method. Comparing two nodes of different concrete types
	// always returns false rather than panicking.
	EquivalentTo(other Node) bool

	// Describe returns a short, human-readable summary of this node for
	// debugging and plan-explain output. It does not include children.
	Describe() string
}

// QueryPlan is a query plan graph produced by FromExpr, rooted at Root.
type QueryPlan struct {
	// Root is the root node of the plan graph.
	Root Node
}

// String returns Root's description, or "<nil>" if p is nil or has no root.
func (p *QueryPlan) String() string {
	if p == nil || p.Root == nil {
		return "<nil>"
	}
	return p.Root.Describe()
}

// baseNode implements the child-bookkeeping portion of Node (ChildCount,
// Child, SetChildren, ReplaceChild, ParentCount) so concrete node types only
// need to embed it and implement EquivalentTo and Describe themselves.
type baseNode struct {
	children []Node
	parents  int

	// occurrences records, for every real parser.Expr occurrence that was
	// canonicalized onto this node (by CommonSubexpressionElimination), how
	// to materialize that occurrence back onto the real parser.Expr tree.
	// A node built by FromExpr starts with exactly one occurrence (FromExpr
	// never introduces sharing); cseCanonicalizer.canonicalize appends a
	// merged node's occurrences onto its survivor's slice so none are lost.
	// See materialize.go's occurrenceRecord and MaterializeSharing.
	occurrences []occurrenceRecord
}

// addOccurrence appends o to b's occurrence list.
func (b *baseNode) addOccurrence(o occurrenceRecord) {
	b.occurrences = append(b.occurrences, o)
}

// addOccurrences appends every element of os to b's occurrence list, in
// order. Used by CommonSubexpressionElimination to transfer a merged node's
// occurrences onto its surviving canonical node.
func (b *baseNode) addOccurrences(os []occurrenceRecord) {
	b.occurrences = append(b.occurrences, os...)
}

// occurrenceRecords returns b's occurrence list. The returned slice must
// not be mutated by the caller.
func (b *baseNode) occurrenceRecords() []occurrenceRecord {
	return b.occurrences
}

// ChildCount returns the number of direct children of this node.
func (b *baseNode) ChildCount() int {
	return len(b.children)
}

// Child returns the direct child at idx, which must be in
// [0, ChildCount()). Child panics if idx is out of range.
func (b *baseNode) Child(idx int) Node {
	return b.children[idx]
}

// SetChildren replaces this node's entire list of children with children,
// adjusting the ParentCount of every removed and added child accordingly.
func (b *baseNode) SetChildren(children []Node) {
	for _, old := range b.children {
		decrementParents(old)
	}
	b.children = children
	for _, child := range b.children {
		incrementParents(child)
	}
}

// ReplaceChild replaces the first occurrence of oldChild with newChild among
// this node's children, adjusting ParentCount accordingly. It panics if
// oldChild is not a child of this node.
func (b *baseNode) ReplaceChild(oldChild, newChild Node) {
	for i, child := range b.children {
		if child == oldChild {
			if oldChild == newChild {
				return
			}
			b.children[i] = newChild
			decrementParents(oldChild)
			incrementParents(newChild)
			return
		}
	}
	panic(fmt.Sprintf("plan: ReplaceChild: %v is not a child of this node", oldChild))
}

// ParentCount reports how many child slots, across the plan graph, currently
// reference this node.
func (b *baseNode) ParentCount() int {
	return b.parents
}

// nodeWithParents is implemented by any Node that embeds baseNode; it is
// used by incrementParents/decrementParents so they don't need to duplicate
// the concrete-type switch that Node itself avoids.
type nodeWithParents interface {
	Node
	adjustParents(delta int)
}

// adjustParents adjusts b's parent count by delta.
func (b *baseNode) adjustParents(delta int) {
	b.parents += delta
}

func incrementParents(n Node) {
	if n == nil {
		return
	}
	if p, ok := n.(nodeWithParents); ok {
		p.adjustParents(1)
	}
}

func decrementParents(n Node) {
	if n == nil {
		return
	}
	if p, ok := n.(nodeWithParents); ok {
		p.adjustParents(-1)
	}
}
