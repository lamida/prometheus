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
)

func TestPropagateMatchers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "copies matcher from LHS to RHS missing it",
			input: `up{job="a"} == on(job) down`,
			want:  `up{job="a"} == on(job) down{job="a"}`,
		},
		{
			name:  "copies matcher from RHS to LHS missing it",
			input: `up == on(job) down{job="a"}`,
			want:  `up{job="a"} == on(job) down{job="a"}`,
		},
		{
			name:  "both sides already have a matcher on the label: untouched",
			input: `up{job="a"} == on(job) down{job="b"}`,
			want:  `up{job="a"} == on(job) down{job="b"}`,
		},
		{
			name:  "neither side has a matcher on the matched label: untouched",
			input: `up == on(job) down`,
			want:  `up == on(job) down`,
		},
		{
			name:  "ignoring(...) is left untouched",
			input: `up{job="a"} == ignoring(job) down`,
			want:  `up{job="a"} == ignoring(job) down`,
		},
		{
			name:  "many-to-one is left untouched",
			input: `up{job="a"} == on(job) group_left() down`,
			want:  `up{job="a"} == on(job) group_left() down`,
		},
		{
			name:  "one side is not a bare selector: untouched",
			input: `sum(up{job="a"}) == on(job) down`,
			want:  `sum(up{job="a"}) == on(job) down`,
		},
		{
			name:  "matcher on an unmatched label is not propagated",
			input: `up{instance="0"} == on(job) down`,
			want:  `up{instance="0"} == on(job) down`,
		},
		{
			name:  "propagates through a nested binary expression",
			input: `(up{job="a"} == on(job) down) + (foo{job="b"} == on(job) bar)`,
			want:  `(up{job="a"} == on(job) down{job="a"}) + (foo{job="b"} == on(job) bar{job="b"})`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := testParser.ParseExpr(tc.input)
			require.NoError(t, err)
			want, err := testParser.ParseExpr(tc.want)
			require.NoError(t, err)

			got, err := PropagateMatchers{}.Apply(context.Background(), input, time.Time{}, time.Time{}, 0)
			require.NoError(t, err)
			require.Equal(t, want.String(), got.String())
		})
	}
}
