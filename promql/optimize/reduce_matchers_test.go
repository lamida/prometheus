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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/parser"
)

func TestReduceMatchers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no duplicates, unchanged",
			input: `up{job="a"}`,
			want:  `up{job="a"}`,
		},
		{
			name:  "exact duplicate on selector",
			input: `up{job="a", job="a"}`,
			want:  `up{job="a"}`,
		},
		{
			name:  "duplicate does not collapse different match types on the same label",
			input: `up{job="a", job!="a"}`,
			want:  `up{job="a", job!="a"}`,
		},
		{
			name:  "duplicate inside a binary expression",
			input: `up{job="a", job="a"} / down{env="prod", env="prod"}`,
			want:  `up{job="a"} / down{env="prod"}`,
		},
		{
			name:  "duplicate inside an aggregation",
			input: `sum(up{job="a", job="a"}) by (job)`,
			want:  `sum(up{job="a"}) by (job)`,
		},
		{
			name:  "duplicate inside a range/matrix selector",
			input: `rate(up{job="a", job="a"}[5m])`,
			want:  `rate(up{job="a"}[5m])`,
		},
		{
			name:  "duplicate inside a subquery",
			input: `rate(up{job="a", job="a"}[5m])[10m:1m]`,
			want:  `rate(up{job="a"}[5m])[10m:1m]`,
		},
		{
			name:  "three-way duplicate collapses to one",
			input: `up{job="a", job="a", job="a"}`,
			want:  `up{job="a"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := testParser.ParseExpr(tc.input)
			require.NoError(t, err)
			want, err := testParser.ParseExpr(tc.want)
			require.NoError(t, err)

			got, err := ReduceMatchers{}.Apply(context.Background(), input, time.Time{}, time.Time{}, 0)
			require.NoError(t, err)
			require.Equal(t, want.String(), got.String())
		})
	}
}

func TestDedupeMatchers_DoesNotMutateSharedBackingArray(t *testing.T) {
	expr, err := testParser.ParseExpr(`up{job="a", job="a"} / up{job="a", job="a"}`)
	require.NoError(t, err)
	bin := expr.(*parser.BinaryExpr)
	lhs := bin.LHS.(*parser.VectorSelector)
	rhs := bin.RHS.(*parser.VectorSelector)
	// Each selector's LabelMatchers holds the implicit __name__="up" matcher
	// plus the two duplicate job="a" matchers.
	require.Len(t, lhs.LabelMatchers, 3)
	require.Len(t, rhs.LabelMatchers, 3)

	_, err = ReduceMatchers{}.Apply(context.Background(), expr, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)

	require.Len(t, lhs.LabelMatchers, 2)
	require.Len(t, rhs.LabelMatchers, 2)
}
