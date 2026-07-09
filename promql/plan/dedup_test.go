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

	"github.com/stretchr/testify/require"
)

// TestFromExpr_DeduplicateAndMergeInsertion covers the insertion side of
// EliminateDeduplicateAndMerge: which BinaryExpr/Call/UnaryExpr nodes
// FromExpr wraps in a DeduplicateAndMergeNode.
func TestFromExpr_DeduplicateAndMergeInsertion(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantWrap  bool
		checkRoot func(t *testing.T, root Node) Node // returns the (possibly unwrapped) inner node for further assertions.
	}{
		{
			name:     "rate is wrapped",
			query:    "rate(foo[5m])",
			wantWrap: true,
		},
		{
			name:     "logical or is wrapped",
			query:    "foo or bar",
			wantWrap: true,
		},
		{
			name:     "comparison with bool modifier is wrapped",
			query:    "foo > bool 5",
			wantWrap: true,
		},
		{
			name:     "comparison filter without bool modifier is not wrapped",
			query:    "foo > 5",
			wantWrap: false,
		},
		{
			name:     "unary minus of a vector is wrapped",
			query:    "-foo",
			wantWrap: true,
		},
		{
			name:     "label_replace is wrapped",
			query:    `label_replace(foo, "a", "$1", "b", "(.*)")`,
			wantWrap: true,
		},
		{
			name:     "plain selector is not wrapped",
			query:    "foo",
			wantWrap: false,
		},
		{
			name:     "aggregate is not wrapped",
			query:    "sum(foo)",
			wantWrap: false,
		},
		{
			name:     "vector-vector arithmetic is not wrapped",
			query:    "foo + bar",
			wantWrap: false,
		},
		{
			name:     "arithmetic vector-scalar drops name and is wrapped",
			query:    "foo + 5",
			wantWrap: true,
		},
		{
			name:     "unary minus of a scalar is not wrapped",
			query:    "-(1)",
			wantWrap: false,
		},
		{
			name:     "instant function that drops the name is wrapped",
			query:    "abs(foo)",
			wantWrap: true,
		},
		{
			name:     "sort does not need a wrap",
			query:    "sort(foo)",
			wantWrap: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expr := preprocess(t, tc.query)
			p, err := FromExpr(expr)
			require.NoError(t, err)

			_, isDedup := p.Root.(*DeduplicateAndMergeNode)
			require.Equal(t, tc.wantWrap, isDedup, "plan: %s", p.String())
		})
	}
}

// TestEliminateDeduplicateAndMerge covers the elimination side, for both
// delayedNameRemoval settings where they differ.
func TestEliminateDeduplicateAndMerge(t *testing.T) {
	tests := []struct {
		name                   string
		query                  string
		wantEliminatedNonDelay bool
		wantEliminatedDelay    bool
	}{
		{
			// rate(foo[5m]): foo has an exact __name__ matcher, so the
			// wrap is provably unnecessary in both modes.
			name:                   "rate with exact name matcher is eliminated",
			query:                  "rate(foo[5m])",
			wantEliminatedNonDelay: true,
			wantEliminatedDelay:    true,
		},
		{
			// `or` can always produce colliding labelsets, regardless of
			// mode.
			name:                   "logical or is never eliminated",
			query:                  "foo or bar",
			wantEliminatedNonDelay: false,
			wantEliminatedDelay:    false,
		},
		{
			// label_replace can always rewrite a label to collide with
			// another series, regardless of mode.
			name:                   "label_replace is never eliminated",
			query:                  `label_replace(foo, "a", "$1", "b", "(.*)")`,
			wantEliminatedNonDelay: false,
			wantEliminatedDelay:    false,
		},
		{
			// A selector without an exact name matcher, wrapped by an
			// outer unary minus: not eliminable in non-delayed mode, since
			// nothing proves the underlying selection is unique. In
			// delayed mode it IS eliminated: canEliminateDeduplicateAndMergeDelayedNameRemoval
			// defaults to true for UnaryExprNode (Mimir's own
			// canEliminateDeduplicateAndMergeDelayedNameRemoval switch only
			// special-cases BinaryExpression/DropName/FunctionCall; a bare
			// UnaryExpression falls through to its default:true case),
			// because in delayed mode the real protection against a
			// same-labelset collision is deferred to a single wrap at the
			// root of the whole query (Mimir's insertDropNameOperator,
			// which this package deliberately does not port — see
			// canEliminateDeduplicateAndMergeDelayedNameRemoval's doc), not
			// to any per-operator wrap along the way.
			name:                   "selector without exact name matcher is not eliminated in non-delayed mode",
			query:                  `-{job="x"}`,
			wantEliminatedNonDelay: false,
			wantEliminatedDelay:    true,
		},
		{
			// Same reasoning as above, with a regex name matcher (which
			// doesn't count as "exact" either) instead of no name matcher
			// at all.
			name:                   "regex name matcher is not an exact match in non-delayed mode",
			query:                  `-{__name__=~"foo.*"}`,
			wantEliminatedNonDelay: false,
			wantEliminatedDelay:    true,
		},
		{
			// Arithmetic vector-scalar: not eliminated in non-delayed mode
			// (canEliminateDeduplicateAndMerge always returns false for a
			// non-retaining vector-scalar BinaryExprNode, regardless of the
			// vector operand), but eliminated in delayed mode (only `or`
			// disqualifies a BinaryExprNode there).
			name:                   "arithmetic vector-scalar differs by mode",
			query:                  "rate(foo[5m]) + 5",
			wantEliminatedNonDelay: false,
			wantEliminatedDelay:    true,
		},
		{
			// abs(rate(foo[5m])): rate(foo[5m]) is itself eliminable, and
			// abs is not a label-modifying function, so the outer wrap is
			// eliminable once foo's exact name matcher is established.
			name:                   "nested non-label-modifying calls are eliminated",
			query:                  "abs(rate(foo[5m]))",
			wantEliminatedNonDelay: true,
			wantEliminatedDelay:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// EliminateDeduplicateAndMerge mutates its input tree in place
			// (via ReplaceChild), so each mode needs its own freshly-built
			// plan rather than sharing one FromExpr result between them.
			t.Run("non-delayed", func(t *testing.T) {
				expr := preprocess(t, tc.query)
				p, err := FromExpr(expr)
				require.NoError(t, err)

				root, eliminated := EliminateDeduplicateAndMerge(p.Root, false)
				_, stillWrapped := root.(*DeduplicateAndMergeNode)
				require.Equal(t, tc.wantEliminatedNonDelay, eliminated > 0 && !stillWrapped)
				require.Equal(t, !tc.wantEliminatedNonDelay, stillWrapped)
			})

			t.Run("delayed", func(t *testing.T) {
				expr := preprocess(t, tc.query)
				p, err := FromExpr(expr)
				require.NoError(t, err)

				root, eliminated := EliminateDeduplicateAndMerge(p.Root, true)
				_, stillWrapped := root.(*DeduplicateAndMergeNode)
				require.Equal(t, tc.wantEliminatedDelay, eliminated > 0 && !stillWrapped)
				require.Equal(t, !tc.wantEliminatedDelay, stillWrapped)
			})
		})
	}
}

// TestEliminateDeduplicateAndMerge_RoundTrip runs a handful of realistic
// full queries through FromExpr followed by EliminateDeduplicateAndMerge,
// and asserts the final plan shape at the spot a DeduplicateAndMergeNode
// would appear.
func TestEliminateDeduplicateAndMerge_RoundTrip(t *testing.T) {
	t.Run("sum(rate(foo[5m])) by (job) has no surviving wrap", func(t *testing.T) {
		expr := preprocess(t, "sum(rate(foo[5m])) by (job)")
		p, err := FromExpr(expr)
		require.NoError(t, err)

		root, eliminated := EliminateDeduplicateAndMerge(p.Root, false)
		require.Equal(t, 1, eliminated)

		ae, ok := root.(*AggregateExprNode)
		require.True(t, ok, "expected *AggregateExprNode root, got %T", root)
		_, ok = ae.Child(0).(*CallNode)
		require.True(t, ok, "expected rate() directly under sum(), got %T", ae.Child(0))
	})

	t.Run("label_replace(rate(foo[5m]), ...) keeps its wrap", func(t *testing.T) {
		expr := preprocess(t, `label_replace(rate(foo[5m]), "a", "$1", "b", "(.*)")`)
		p, err := FromExpr(expr)
		require.NoError(t, err)

		root, eliminated := EliminateDeduplicateAndMerge(p.Root, false)
		// The inner rate(foo[5m]) wrap is eliminated, but the outer
		// label_replace wrap is not.
		require.Equal(t, 1, eliminated)

		dedup, ok := root.(*DeduplicateAndMergeNode)
		require.True(t, ok, "expected surviving *DeduplicateAndMergeNode root, got %T", root)
		call, ok := dedup.Child(0).(*CallNode)
		require.True(t, ok, "expected *CallNode under the surviving wrap, got %T", dedup.Child(0))
		require.Equal(t, "label_replace", call.Func.Name)
	})

	t.Run("foo or bar keeps its wrap", func(t *testing.T) {
		expr := preprocess(t, "foo or bar")
		p, err := FromExpr(expr)
		require.NoError(t, err)

		root, eliminated := EliminateDeduplicateAndMerge(p.Root, false)
		require.Equal(t, 0, eliminated)

		_, ok := root.(*DeduplicateAndMergeNode)
		require.True(t, ok, "expected surviving *DeduplicateAndMergeNode root, got %T", root)
	})

	t.Run("-abs(rate(foo[5m])) eliminates all three wraps", func(t *testing.T) {
		// rate(foo[5m]) is wrapped (range-vector function dropping the
		// name), abs(...) is wrapped on top (instant function dropping the
		// name, not label-modifying), and the unary minus is wrapped on
		// top of that (negating a vector). None of the three arguments
		// involved is a scalar literal, so this avoids the
		// NumberLiteralNode-argument conservatism documented on
		// canEliminateDeduplicateAndMerge (see e.g. clamp(foo, 0, 1), which
		// can never fully eliminate because of its scalar min/max
		// arguments) and all three wraps should be eliminated once foo's
		// exact name matcher is established.
		expr := preprocess(t, "-abs(rate(foo[5m]))")
		p, err := FromExpr(expr)
		require.NoError(t, err)

		root, eliminated := EliminateDeduplicateAndMerge(p.Root, false)
		require.Equal(t, 3, eliminated, "expected the rate(), abs(), and unary-minus wraps all eliminated")

		unary, ok := root.(*UnaryExprNode)
		require.True(t, ok, "expected *UnaryExprNode root (the unary minus operator itself, whose own wrap was eliminated), got %T", root)

		abs, ok := unary.Child(0).(*CallNode)
		require.True(t, ok, "expected *CallNode (abs) under the unary minus, got %T", unary.Child(0))
		require.Equal(t, "abs", abs.Func.Name)

		rate, ok := abs.Child(0).(*CallNode)
		require.True(t, ok, "expected *CallNode (rate) under abs, got %T", abs.Child(0))
		require.Equal(t, "rate", rate.Func.Name)
	})
}
