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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

func newSelector(name string) *VectorSelectorNode {
	return &VectorSelectorNode{Name: name}
}

// TestParentCount_SetChildren_Basic builds a small hand-constructed graph
// (FromExpr never introduces sharing, so bookkeeping across multiple
// parents can only be exercised by building the graph directly) and checks
// ParentCount bookkeeping across a sequence of SetChildren/ReplaceChild
// calls.
func TestParentCount_SetChildren_Basic(t *testing.T) {
	shared := newSelector("shared")
	require.Equal(t, 0, shared.ParentCount())

	parent1 := &UnaryExprNode{Op: parser.SUB}
	parent1.SetChildren([]Node{shared})
	require.Equal(t, 1, shared.ParentCount())

	parent2 := &UnaryExprNode{Op: parser.SUB}
	parent2.SetChildren([]Node{shared})
	require.Equal(t, 2, shared.ParentCount(), "two distinct parents both referencing shared should give ParentCount 2")

	// Detach parent1 from shared by giving it a different child.
	other := newSelector("other")
	parent1.SetChildren([]Node{other})
	require.Equal(t, 1, shared.ParentCount(), "removing shared from parent1's children should decrement shared's ParentCount")
	require.Equal(t, 1, other.ParentCount())

	// Detach parent2 too.
	parent2.SetChildren(nil)
	require.Equal(t, 0, shared.ParentCount())
}

// TestParentCount_SetChildren_SameListTwice verifies that calling
// SetChildren twice with an equal (but freshly allocated) slice of the same
// children leaves ParentCount unchanged: the old edges are removed and the
// same edges are immediately re-added.
func TestParentCount_SetChildren_SameListTwice(t *testing.T) {
	child := newSelector("child")
	parent := &UnaryExprNode{Op: parser.SUB}

	parent.SetChildren([]Node{child})
	require.Equal(t, 1, child.ParentCount())

	parent.SetChildren([]Node{child})
	require.Equal(t, 1, child.ParentCount(), "setting the same children again should not change ParentCount")
}

// TestParentCount_SetChildren_DuplicateChildInOneCall documents and tests
// the decision that a child may legally appear more than once in a single
// SetChildren call (e.g. a degenerate `foo + foo` before CSE has run to
// deduplicate it into real sharing): ParentCount is incremented once per
// occurrence, matching the number of child slots that reference it.
func TestParentCount_SetChildren_DuplicateChildInOneCall(t *testing.T) {
	child := newSelector("child")
	parent := &BinaryExprNode{Op: parser.ADD}

	parent.SetChildren([]Node{child, child})
	require.Equal(t, 2, parent.ChildCount())
	require.Equal(t, 2, child.ParentCount(), "a child referenced twice by one parent should have ParentCount 2")

	// Removing the duplicate-referencing parent entirely should bring
	// ParentCount back to 0, not -1 or 1.
	parent.SetChildren(nil)
	require.Equal(t, 0, child.ParentCount())
}

// TestReplaceChild_Basic verifies ParentCount bookkeeping across
// ReplaceChild calls, including replacing with a brand new node and
// replacing back to a previously-detached node.
func TestReplaceChild_Basic(t *testing.T) {
	a := newSelector("a")
	b := newSelector("b")
	parent := &UnaryExprNode{Op: parser.SUB}
	parent.SetChildren([]Node{a})
	require.Equal(t, 1, a.ParentCount())
	require.Equal(t, 0, b.ParentCount())

	parent.ReplaceChild(a, b)
	require.Equal(t, 0, a.ParentCount())
	require.Equal(t, 1, b.ParentCount())
	require.Same(t, b, parent.Child(0))

	// Replace back to a.
	parent.ReplaceChild(b, a)
	require.Equal(t, 1, a.ParentCount())
	require.Equal(t, 0, b.ParentCount())
}

// TestReplaceChild_WithItself verifies that replacing a child with itself is
// a documented no-op: ParentCount must not change.
func TestReplaceChild_WithItself(t *testing.T) {
	child := newSelector("child")
	parent := &UnaryExprNode{Op: parser.SUB}
	parent.SetChildren([]Node{child})
	require.Equal(t, 1, child.ParentCount())

	parent.ReplaceChild(child, child)
	require.Equal(t, 1, child.ParentCount(), "replacing a child with itself must not change ParentCount")
	require.Same(t, child, parent.Child(0))
}

// TestReplaceChild_FirstOccurrenceOnly verifies that when oldChild appears
// more than once among a parent's children, ReplaceChild only replaces the
// first occurrence, per its documented contract.
func TestReplaceChild_FirstOccurrenceOnly(t *testing.T) {
	child := newSelector("child")
	replacement := newSelector("replacement")
	parent := &BinaryExprNode{Op: parser.ADD}
	parent.SetChildren([]Node{child, child})
	require.Equal(t, 2, child.ParentCount())

	parent.ReplaceChild(child, replacement)
	require.Equal(t, 1, child.ParentCount(), "only the first occurrence should be replaced")
	require.Equal(t, 1, replacement.ParentCount())
	require.Same(t, replacement, parent.Child(0))
	require.Same(t, child, parent.Child(1))
}

// TestReplaceChild_NotAChildPanics verifies ReplaceChild's documented panic
// when oldChild is not actually among the parent's children.
func TestReplaceChild_NotAChildPanics(t *testing.T) {
	parent := &UnaryExprNode{Op: parser.SUB}
	parent.SetChildren([]Node{newSelector("child")})

	require.Panics(t, func() {
		parent.ReplaceChild(newSelector("not-a-child"), newSelector("replacement"))
	})
}

func TestEquivalentTo_VectorSelector(t *testing.T) {
	m1 := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "job", "a")}
	m2 := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "job", "a")}
	m3 := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "job", "b")}

	a := &VectorSelectorNode{Name: "foo", LabelMatchers: m1, Offset: time.Minute}
	b := &VectorSelectorNode{Name: "foo", LabelMatchers: m2, Offset: time.Minute}
	require.True(t, a.EquivalentTo(b))
	require.True(t, b.EquivalentTo(a))

	differentName := &VectorSelectorNode{Name: "bar", LabelMatchers: m1, Offset: time.Minute}
	require.False(t, a.EquivalentTo(differentName))

	differentMatchers := &VectorSelectorNode{Name: "foo", LabelMatchers: m3, Offset: time.Minute}
	require.False(t, a.EquivalentTo(differentMatchers))

	differentOffset := &VectorSelectorNode{Name: "foo", LabelMatchers: m1, Offset: 2 * time.Minute}
	require.False(t, a.EquivalentTo(differentOffset))

	ts1 := int64(100)
	ts2 := int64(200)
	differentTimestamp := &VectorSelectorNode{Name: "foo", LabelMatchers: m1, Offset: time.Minute, Timestamp: &ts1}
	sameTimestampValue := &VectorSelectorNode{Name: "foo", LabelMatchers: m1, Offset: time.Minute, Timestamp: &ts1}
	otherTimestampValue := &VectorSelectorNode{Name: "foo", LabelMatchers: m1, Offset: time.Minute, Timestamp: &ts2}
	require.False(t, a.EquivalentTo(differentTimestamp), "nil timestamp vs set timestamp must differ")
	require.True(t, differentTimestamp.EquivalentTo(sameTimestampValue), "equal timestamp values behind different pointers must be equivalent")
	require.False(t, differentTimestamp.EquivalentTo(otherTimestampValue))
}

func TestEquivalentTo_MatrixSelector(t *testing.T) {
	a := &MatrixSelectorNode{Range: 5 * time.Minute}
	b := &MatrixSelectorNode{Range: 5 * time.Minute}
	c := &MatrixSelectorNode{Range: 10 * time.Minute}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

func TestEquivalentTo_BinaryExpr(t *testing.T) {
	a := &BinaryExprNode{Op: parser.ADD}
	b := &BinaryExprNode{Op: parser.ADD}
	c := &BinaryExprNode{Op: parser.SUB}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))

	withBool := &BinaryExprNode{Op: parser.EQLC, ReturnBool: true}
	withoutBool := &BinaryExprNode{Op: parser.EQLC, ReturnBool: false}
	require.False(t, withBool.EquivalentTo(withoutBool))

	vm1 := &parser.VectorMatching{Card: parser.CardOneToOne, On: true, MatchingLabels: []string{"job"}}
	vm2 := &parser.VectorMatching{Card: parser.CardOneToOne, On: true, MatchingLabels: []string{"job"}}
	vm3 := &parser.VectorMatching{Card: parser.CardOneToOne, On: true, MatchingLabels: []string{"instance"}}
	withVM1 := &BinaryExprNode{Op: parser.ADD, VectorMatching: vm1}
	withVM2 := &BinaryExprNode{Op: parser.ADD, VectorMatching: vm2}
	withVM3 := &BinaryExprNode{Op: parser.ADD, VectorMatching: vm3}
	require.True(t, withVM1.EquivalentTo(withVM2))
	require.False(t, withVM1.EquivalentTo(withVM3))
	require.False(t, withVM1.EquivalentTo(a), "nil VectorMatching vs non-nil must differ")
}

func TestEquivalentTo_AggregateExpr(t *testing.T) {
	a := &AggregateExprNode{Op: parser.SUM, Grouping: []string{"job"}}
	b := &AggregateExprNode{Op: parser.SUM, Grouping: []string{"job"}}
	c := &AggregateExprNode{Op: parser.SUM, Grouping: []string{"instance"}}
	d := &AggregateExprNode{Op: parser.MAX, Grouping: []string{"job"}}
	e := &AggregateExprNode{Op: parser.SUM, Grouping: []string{"job"}, Without: true}
	f := &AggregateExprNode{Op: parser.TOPK, Grouping: []string{"job"}, HasParam: true}

	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
	require.False(t, a.EquivalentTo(d))
	require.False(t, a.EquivalentTo(e))
	require.False(t, a.EquivalentTo(f))
}

func TestEquivalentTo_Call(t *testing.T) {
	rate := &parser.Function{Name: "rate"}
	rate2 := &parser.Function{Name: "rate"}
	irate := &parser.Function{Name: "irate"}

	a := &CallNode{Func: rate}
	b := &CallNode{Func: rate2}
	c := &CallNode{Func: irate}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

func TestEquivalentTo_SubqueryExpr(t *testing.T) {
	a := &SubqueryExprNode{Range: 5 * time.Minute, Step: time.Minute}
	b := &SubqueryExprNode{Range: 5 * time.Minute, Step: time.Minute}
	c := &SubqueryExprNode{Range: 10 * time.Minute, Step: time.Minute}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

func TestEquivalentTo_NumberLiteral(t *testing.T) {
	a := &NumberLiteralNode{Val: 1}
	b := &NumberLiteralNode{Val: 1}
	c := &NumberLiteralNode{Val: 2}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

func TestEquivalentTo_StringLiteral(t *testing.T) {
	a := &StringLiteralNode{Val: "x"}
	b := &StringLiteralNode{Val: "x"}
	c := &StringLiteralNode{Val: "y"}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

func TestEquivalentTo_UnaryExpr(t *testing.T) {
	a := &UnaryExprNode{Op: parser.SUB}
	b := &UnaryExprNode{Op: parser.SUB}
	c := &UnaryExprNode{Op: parser.ADD}
	require.True(t, a.EquivalentTo(b))
	require.False(t, a.EquivalentTo(c))
}

// TestEquivalentTo_CrossType verifies that comparing nodes of different
// concrete types returns false rather than panicking, for every pair of
// node types this package defines.
func TestEquivalentTo_CrossType(t *testing.T) {
	nodes := []Node{
		&VectorSelectorNode{Name: "foo"},
		&MatrixSelectorNode{Range: time.Minute},
		&BinaryExprNode{Op: parser.ADD},
		&AggregateExprNode{Op: parser.SUM},
		&CallNode{Func: &parser.Function{Name: "rate"}},
		&SubqueryExprNode{Range: time.Minute},
		&NumberLiteralNode{Val: 1},
		&StringLiteralNode{Val: "x"},
		&UnaryExprNode{Op: parser.SUB},
	}
	for i, n1 := range nodes {
		for j, n2 := range nodes {
			if i == j {
				continue
			}
			require.NotPanics(t, func() {
				require.False(t, n1.EquivalentTo(n2), "%T should not be EquivalentTo %T", n1, n2)
			})
		}
	}
}

func TestQueryPlan_String(t *testing.T) {
	var nilPlan *QueryPlan
	require.Equal(t, "<nil>", nilPlan.String())

	empty := &QueryPlan{}
	require.Equal(t, "<nil>", empty.String())

	p := &QueryPlan{Root: newSelector("foo")}
	require.Contains(t, p.String(), "foo")
}
