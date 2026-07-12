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

package plan_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/plan"
)

// buildCSEd parses+preprocesses query, builds its plan, runs
// NormalizeForCSE and CommonSubexpressionElimination over it — the
// prerequisite state SubsetSelectorElimination requires (see its doc) —
// and returns the resulting root.
func buildCSEd(t *testing.T, query string) plan.Node {
	t.Helper()
	root := buildAndPrepare(t, query)
	root, _ = plan.CommonSubexpressionElimination(root)
	return root
}

// Note on matcher counts in this file: a selector like `up{job="a"}` carries
// an implicit __name__="up" matcher alongside the explicit ones in
// LabelMatchers (parser.VectorSelector.Name is a separate field, but the
// name is ALSO present as a matcher), so `up{job="a"}` has 2 matchers, not
// 1, and `up{job="a",env="prod"}` has 3, not 2.

// TestSubsetSelectorElimination_BasicSubsumption covers the straightforward
// case from docs/query-planner-phase4-design.md §1: a selector whose
// matchers are a strict superset of another's should point its subsetSource
// at the narrower one.
func TestSubsetSelectorElimination_BasicSubsumption(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + up{job="a",env="prod"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 1, found)

	selectors := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectors, 2)

	var wide, narrow *plan.VectorSelectorNode
	for _, s := range selectors {
		if len(s.LabelMatchers) == 2 {
			wide = s
		} else {
			narrow = s
		}
	}
	require.NotNil(t, wide)
	require.NotNil(t, narrow)
	require.Same(t, wide, narrow.SubsetSource(), "expected the wider selector's node to be the narrower one's subsetSource")
	require.Nil(t, wide.SubsetSource(), "the wider selector has no cheaper superset available")
}

// TestSubsetSelectorElimination_NoRelationForUnrelatedSelectors ensures two
// selectors with disjoint matcher sets (neither a superset of the other)
// are left untouched.
func TestSubsetSelectorElimination_NoRelationForUnrelatedSelectors(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + up{job="b"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 0, found)

	for _, s := range findAll[*plan.VectorSelectorNode](root) {
		require.Nil(t, s.SubsetSource())
	}
}

// TestSubsetSelectorElimination_PicksCheapestSource covers §2's "cheapest
// already-materialized superset" rule: when more than one eligible source
// subsumes a selector, the one with fewer matchers (cheaper to have
// produced) wins.
func TestSubsetSelectorElimination_PicksCheapestSource(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + up{job="a",env="prod"} + up{job="a",env="prod",region="us"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 2, found)

	selectors := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectors, 3)

	var one, two, three *plan.VectorSelectorNode
	for _, s := range selectors {
		switch len(s.LabelMatchers) {
		case 2:
			one = s
		case 3:
			two = s
		case 4:
			three = s
		}
	}
	require.NotNil(t, one)
	require.NotNil(t, two)
	require.NotNil(t, three)

	require.Same(t, one, two.SubsetSource(), "the two-explicit-matcher selector's cheapest available source is the one-explicit-matcher selector")
	require.Same(t, one, three.SubsetSource(), "the three-explicit-matcher selector's cheapest available source is the one-explicit-matcher selector, not the two-explicit-matcher one")
}

// TestSubsetSelectorElimination_IncomparableSupersets covers §2's partial
// order point directly: two selectors that each subsume a third, but
// neither subsumes the other, both remain valid candidates and one is
// picked deterministically.
func TestSubsetSelectorElimination_IncomparableSupersets(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + up{env="prod"} + up{job="a",env="prod"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 1, found, "exactly one selector (the three-matcher one) should get a subsetSource")

	selectors := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectors, 3)

	for _, s := range selectors {
		if len(s.LabelMatchers) == 3 {
			require.NotNil(t, s.SubsetSource(), "the three-matcher selector should have picked one of the two two-matcher selectors")
			require.Len(t, s.SubsetSource().LabelMatchers, 2)
		} else {
			require.Nil(t, s.SubsetSource(), "neither two-matcher selector subsumes the other")
		}
	}
}

// TestSubsetSelectorElimination_DifferentNameNeverSubsumes ensures two
// selectors for different metric names are never related, regardless of
// matcher overlap.
func TestSubsetSelectorElimination_DifferentNameNeverSubsumes(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + down{job="a",env="prod"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 0, found)
}

// TestSubsetSelectorElimination_DifferentOffsetNeverSubsumes ensures two
// otherwise-matching selectors at different offsets are never related: an
// offset difference means they are not looking at the same point in time,
// so no subset relation can be assumed. This package's preprocess test
// helper never calls promql/engine.go's unexported setOffsetForAtModifier
// (see TestCommonSubexpressionElimination_DifferentOffsetsDoNotMerge's doc
// in cse_test.go for why), so Offset is set by hand here.
func TestSubsetSelectorElimination_DifferentOffsetNeverSubsumes(t *testing.T) {
	root := buildCSEd(t, `up{job="a"} + up{job="a",env="prod"}`)

	selectors := findAll[*plan.VectorSelectorNode](root)
	require.Len(t, selectors, 2)
	selectors[0].Offset = time.Minute
	selectors[1].Offset = 2 * time.Minute

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 0, found)
}

// TestSubsetSelectorElimination_MatrixNestedSelectorExcluded covers the
// MatrixSelectorNode exclusion documented on SubsetSelectorElimination: a
// selector reachable only as a MatrixSelectorNode's child must never be
// related, in either direction, since promql/engine.go's execution wiring
// never evaluates it through the code path that would consult the relation.
func TestSubsetSelectorElimination_MatrixNestedSelectorExcluded(t *testing.T) {
	root := buildCSEd(t, `rate(up{job="a"}[5m]) + up{job="a",env="prod"}`)

	found := plan.SubsetSelectorElimination(root)
	require.Equal(t, 0, found, "the matrix-nested selector must not participate as a source or a subsumed node")

	for _, s := range findAll[*plan.VectorSelectorNode](root) {
		require.Nil(t, s.SubsetSource())
	}
}

// TestSubsetSelectorElimination_UnstableOffsetNeverSubsumes covers the same
// hasUnstableOffset conservatism CSE's own EquivalentTo applies (Open
// Question 4): a selector with its own @ modifier nested under a subquery
// must never participate in a subsumption relation, in either direction.
func TestSubsetSelectorElimination_UnstableOffsetNeverSubsumes(t *testing.T) {
	root := buildCSEd(t, `sum_over_time((up{job="a"} @ 100)[5m:1m]) + up{job="a",env="prod"}`)

	selectors := findAll[*plan.VectorSelectorNode](root)
	var unstable *plan.VectorSelectorNode
	for _, s := range selectors {
		if s.HasUnstableOffset() {
			unstable = s
		}
	}
	require.NotNil(t, unstable, "expected one selector with an unstable offset in this query shape")

	plan.SubsetSelectorElimination(root)
	require.Nil(t, unstable.SubsetSource())
	for _, s := range selectors {
		require.NotSame(t, unstable, s.SubsetSource(), "an unstable-offset selector must never be picked as a source either")
	}
}
