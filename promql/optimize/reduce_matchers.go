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
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// ReduceMatchers removes exact-duplicate label matchers from every
// VectorSelector in a query, e.g. rewriting {job="a", job="a"} to {job="a"}.
//
// It only removes a matcher when another matcher on the same selector has
// the identical Type, Name, and Value: this is unambiguously safe and needs
// no regex analysis. It deliberately does not attempt to prove that two
// different matchers on the same label subsume one another (for example,
// two overlapping MatchRegexp patterns) — that requires reasoning about
// regex anchoring and .* semantics, which is correctness-sensitive enough
// to deserve its own pass and review cycle.
//
// Removing a duplicate matcher does not change which series a selector
// matches, so this rewrite is always safe: fewer matchers to evaluate per
// series at query time, and, for the leading exact-match case, a cheaper
// index lookup in tsdb/querier.go.
type ReduceMatchers struct{}

// Name implements ASTOptimizationPass.
func (ReduceMatchers) Name() string { return "reduce_matchers" }

// Apply implements ASTOptimizationPass.
func (p ReduceMatchers) Apply(_ context.Context, expr parser.Expr, _, _ time.Time, _ time.Duration) (parser.Expr, error) {
	if err := p.rewrite(expr); err != nil {
		return nil, err
	}
	return expr, nil
}

func (p ReduceMatchers) rewrite(expr parser.Expr) error {
	if vs, ok := expr.(*parser.VectorSelector); ok {
		vs.LabelMatchers = dedupeMatchers(vs.LabelMatchers)
		return nil
	}
	return TransformChildren(expr, func(child parser.Expr) (parser.Expr, error) {
		if err := p.rewrite(child); err != nil {
			return nil, err
		}
		return child, nil
	})
}

// dedupeMatchers returns matchers with exact duplicates removed, preserving
// the order of each matcher's first occurrence.
func dedupeMatchers(matchers []*labels.Matcher) []*labels.Matcher {
	if len(matchers) < 2 {
		return matchers
	}

	type key struct {
		typ   labels.MatchType
		name  string
		value string
	}
	seen := make(map[key]struct{}, len(matchers))
	deduped := matchers[:0:0] // Fresh backing array: matchers may be shared by other selectors.
	for _, m := range matchers {
		k := key{typ: m.Type, name: m.Name, value: m.Value}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		deduped = append(deduped, m)
	}
	if len(deduped) == len(matchers) {
		return matchers
	}
	return deduped
}
