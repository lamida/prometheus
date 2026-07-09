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

import "fmt"

// This file is execution-inert: it only changes the plan graph's shape,
// introducing nodes with ParentCount() > 1 where two or more subtrees are
// structurally identical. Nothing here consults or wires into
// promql/engine.go; nothing yet consumes the sharing this pass introduces.
// See docs/query-planner-phase2-design.md §4 for the (separate, future)
// execution-strategy work that will.

// CommonSubexpressionElimination rewrites root's plan graph in place,
// finding groups of structurally-identical subtrees — same node per
// EquivalentTo, and recursively identical children, since EquivalentTo
// itself never inspects children (see Node.EquivalentTo's doc) — and
// collapsing each group onto one canonical, shared Node, repointing every
// other occurrence's parent at that survivor via ReplaceChild. It returns
// the (possibly new) root of the rewritten graph and the number of nodes
// eliminated by merging.
//
// Callers should run NormalizeForCSE on root first: without it, two
// subtrees that differ only in, say, matcher order will not be detected as
// duplicates, since EquivalentTo compares matcher lists positionally (see
// nodes.go's equalMatchers doc).
//
// Algorithm: a single bottom-up (post-order) walk, hash-consing style.
// Nodes are grouped into buckets keyed by a cheap structural signature
// (concrete type plus Describe()'s output, which by contract never
// inspects children — see Node.Describe's doc); a signature collision just
// means one extra full check within that bucket, never an incorrect merge,
// since the final decision is always EquivalentTo plus a children check,
// never the signature alone. Because the walk is bottom-up, by the time a
// node N is considered for merging, every one of its children has already
// been canonicalized — replaced with its own group's single surviving
// representative, if it had duplicates. That makes comparing two
// candidates' children an O(children count) pointer-identity check
// (Child(i) == Child(i)), never a re-of the same subtree's structure at
// every ancestor level. Each node is visited and canonicalized exactly
// once (memoized in the resolved map), so the whole pass is a single O(n)
// walk (n = node count in the input graph) plus, per node, a constant
// number of bucket comparisons — not the naive O(n²) pairwise comparison
// across the whole plan that comparing every node against every other node
// directly would require.
func CommonSubexpressionElimination(root Node) (Node, int) {
	c := &cseCanonicalizer{
		buckets:  make(map[string][]Node),
		resolved: make(map[Node]Node),
	}
	newRoot := c.canonicalize(root)
	return newRoot, c.merged
}

// cseCanonicalizer holds the state for one CommonSubexpressionElimination
// call: buckets maps a structural signature to the canonical (surviving)
// nodes already established with that signature, and resolved memoizes
// which canonical node a given original node instance was mapped to, so a
// node reachable via more than one path (e.g. one already shared by a
// prior pass) is only canonicalized once.
type cseCanonicalizer struct {
	buckets  map[string][]Node
	resolved map[Node]Node
	merged   int
}

// canonicalize returns n's canonical representative: n itself, if n is the
// first node seen with its structure, or a previously-seen equivalent node
// otherwise. It first canonicalizes n's own children in place (via
// ReplaceChild), so any comparison against n after this call sees already-
// canonical children.
func (c *cseCanonicalizer) canonicalize(n Node) Node {
	if n == nil {
		return nil
	}
	if canon, ok := c.resolved[n]; ok {
		return canon
	}

	for i := 0; i < n.ChildCount(); i++ {
		child := n.Child(i)
		newChild := c.canonicalize(child)
		if newChild != child {
			n.ReplaceChild(child, newChild)
		}
	}

	sig := cseSignature(n)
	for _, cand := range c.buckets[sig] {
		if cseNodesEqual(n, cand) {
			c.resolved[n] = cand
			c.merged++
			return cand
		}
	}

	c.buckets[sig] = append(c.buckets[sig], n)
	c.resolved[n] = n
	return n
}

// cseSignature returns a cheap bucketing key for n, ignoring children:
// concrete type (so two different node types with coincidentally identical
// Describe() output never land in the same bucket) plus Describe()'s own
// output, which by contract already reflects every type-specific field
// EquivalentTo cares about (see Node.Describe's and Node.EquivalentTo's
// docs). A collision within a bucket costs one extra EquivalentTo check,
// never an incorrect merge; see this file's top-of-file algorithm doc.
func cseSignature(n Node) string {
	return fmt.Sprintf("%T|%s", n, n.Describe())
}

// cseNodesEqual reports whether a and b are interchangeable for CSE
// purposes: EquivalentTo agrees, and they have the same number of children,
// each pairwise identical by pointer (safe only because both a and b have
// already had their own children canonicalized by the time this is called
// — see canonicalize's doc).
func cseNodesEqual(a, b Node) bool {
	if !a.EquivalentTo(b) {
		return false
	}
	if a.ChildCount() != b.ChildCount() {
		return false
	}
	for i := 0; i < a.ChildCount(); i++ {
		if a.Child(i) != b.Child(i) {
			return false
		}
	}
	return true
}
