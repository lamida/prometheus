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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/parser/posrange"
)

var testParser = parser.NewParser(parser.Options{})

// countAndMark recursively applies TransformChildren to every reachable
// node in expr, replacing every VectorSelector it finds with a marker
// selector (Name suffixed with "_marked") and counting how many nodes were
// visited overall. It is used to assert that TransformChildren reaches
// every child slot of every concrete parser.Expr type.
func countAndMark(t *testing.T, expr parser.Expr) (parser.Expr, int) {
	t.Helper()
	count := 1 // count expr itself.
	if vs, ok := expr.(*parser.VectorSelector); ok {
		marked := *vs
		marked.Name += "_marked"
		expr = &marked
	}
	err := TransformChildren(expr, func(child parser.Expr) (parser.Expr, error) {
		rewritten, n := countAndMark(t, child)
		count += n
		return rewritten, nil
	})
	require.NoError(t, err)
	return expr, count
}

func TestTransformChildren_IdentityLeavesTreeUnchanged(t *testing.T) {
	queries := []string{
		`up`,
		`rate(http_requests_total[5m])`,
		`sum(rate(http_requests_total[5m])) by (job)`,
		`up{job="a"} / up{job="b"}`,
		`up{job="a"} and up{job="b"} or up{job="c"}`,
		`quantile(0.9, http_request_duration_seconds)`,
		`(up{job="a"})`,
		`-up`,
		`http_requests_total[5m:1m]`,
		`42`,
		`"a string"`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			expr, err := testParser.ParseExpr(q)
			require.NoError(t, err)
			before := expr.String()

			err = TransformChildren(expr, func(child parser.Expr) (parser.Expr, error) {
				return child, nil
			})
			require.NoError(t, err)
			require.Equal(t, before, expr.String())
		})
	}
}

func TestTransformChildren_VisitsEveryReachableSelector(t *testing.T) {
	// Every concrete parser.Expr node type gets exercised at least once:
	// AggregateExpr (with Param), BinaryExpr, Call, SubqueryExpr, ParenExpr,
	// UnaryExpr, MatrixSelector, and the VectorSelector leaves themselves.
	q := `quantile(0.9, sum(rate((up{job="a"})[5m:1m])) by (job)) / -up{job="b"}`
	expr, err := testParser.ParseExpr(q)
	require.NoError(t, err)

	rewritten, count := countAndMark(t, expr)
	require.Greater(t, count, 1, "expected TransformChildren to recurse into more than just the root node")
	require.Equal(t, 2, strings.Count(rewritten.String(), "up_marked"), "expected both VectorSelector leaves to be visited and marked")
}

func TestTransformChildren_PropagatesError(t *testing.T) {
	expr, err := testParser.ParseExpr(`up{job="a"} + up{job="b"}`)
	require.NoError(t, err)

	wantErr := errors.New("boom")
	err = TransformChildren(expr, func(parser.Expr) (parser.Expr, error) {
		return nil, wantErr
	})
	require.ErrorIs(t, err, wantErr)
}

func TestTransformChildren_LeafNodesAreNoOps(t *testing.T) {
	for _, expr := range []parser.Expr{
		&parser.NumberLiteral{Val: 1},
		&parser.StringLiteral{Val: "x"},
		&parser.VectorSelector{Name: "up"},
	} {
		called := false
		err := TransformChildren(expr, func(child parser.Expr) (parser.Expr, error) {
			called = true
			return child, nil
		})
		require.NoError(t, err)
		require.False(t, called, "leaf node %T should have no children to transform", expr)
	}
}

// unhandledExpr is a minimal parser.Expr implementation not recognized by
// TransformChildren's type switch, used to assert it fails closed on an
// unknown node type rather than silently treating it as a leaf.
type unhandledExpr struct{}

func (unhandledExpr) Type() parser.ValueType                { return parser.ValueTypeScalar }
func (unhandledExpr) PromQLExpr()                           {}
func (unhandledExpr) String() string                        { return "unhandled" }
func (unhandledExpr) Pretty(int) string                     { return "unhandled" }
func (unhandledExpr) PositionRange() posrange.PositionRange { return posrange.PositionRange{} }

func TestTransformChildren_UnhandledTypeReturnsError(t *testing.T) {
	err := TransformChildren(unhandledExpr{}, func(child parser.Expr) (parser.Expr, error) {
		return child, nil
	})
	require.Error(t, err)
}
