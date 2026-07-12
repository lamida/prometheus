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

// PropagateMatchers copies label matchers across a one-to-one on(...) binary
// operation so both sides fetch less from storage.
//
// For a binary expression like:
//
//	up{job="a"} == on(instance) down{job="b"}
//
// instance is used to match series between the two selectors, but neither
// side's matchers narrow what the other side fetches. If one side already
// selects on one of the matched labels and the other side has no matcher on
// that same label at all, PropagateMatchers copies the matcher across: since
// on(...) requires the matched labels to be equal between the two sides for
// a pair of series to be joined at all, any additional matcher on a matched
// label can only remove series that would never have joined anyway, so this
// never changes the result, only how much is fetched to compute it.
//
// Scoped narrowly for this first pass: both sides of the binary expression
// must be bare VectorSelectors (no nested aggregation, subquery, or binary
// expression on either side), the match must be on(...) (not ignoring(...)),
// and the cardinality must be one-to-one. ignoring(...) and many-to-one/
// one-to-many/many-to-many matches are deliberately left untouched: ignoring
// excludes an open-ended, unbounded label set rather than naming the ones
// that must match, and higher cardinality sides can legitimately have several
// matchers per matched label value, both of which need separate reasoning
// this pass does not attempt yet.
type PropagateMatchers struct{}

// Name implements ASTOptimizationPass.
func (PropagateMatchers) Name() string { return "propagate_matchers" }

// Apply implements ASTOptimizationPass.
func (p PropagateMatchers) Apply(_ context.Context, expr parser.Expr, _, _ time.Time, _ time.Duration) (parser.Expr, error) {
	if err := p.rewrite(expr); err != nil {
		return nil, err
	}
	return expr, nil
}

func (p PropagateMatchers) rewrite(expr parser.Expr) error {
	if bin, ok := expr.(*parser.BinaryExpr); ok {
		propagateAcrossOnJoin(bin)
	}
	return TransformChildren(expr, func(child parser.Expr) (parser.Expr, error) {
		if err := p.rewrite(child); err != nil {
			return nil, err
		}
		return child, nil
	})
}

// propagateAcrossOnJoin propagates matchers between bin's operands in place,
// if bin is eligible (see PropagateMatchers's doc comment for the exact
// scope). It does not recurse: the caller is responsible for continuing to
// walk bin's operands for nested binary expressions of their own.
func propagateAcrossOnJoin(bin *parser.BinaryExpr) {
	vm := bin.VectorMatching
	if vm == nil || !vm.On || vm.Card != parser.CardOneToOne || len(vm.MatchingLabels) == 0 {
		return
	}
	lhs, ok := bin.LHS.(*parser.VectorSelector)
	if !ok {
		return
	}
	rhs, ok := bin.RHS.(*parser.VectorSelector)
	if !ok {
		return
	}

	for _, label := range vm.MatchingLabels {
		lhsHas := hasMatcherOnLabel(lhs.LabelMatchers, label)
		rhsHas := hasMatcherOnLabel(rhs.LabelMatchers, label)
		switch {
		case lhsHas && !rhsHas:
			rhs.LabelMatchers = append(rhs.LabelMatchers, copyMatchersOnLabel(lhs.LabelMatchers, label)...)
		case rhsHas && !lhsHas:
			lhs.LabelMatchers = append(lhs.LabelMatchers, copyMatchersOnLabel(rhs.LabelMatchers, label)...)
		}
	}
}

func hasMatcherOnLabel(matchers []*labels.Matcher, name string) bool {
	for _, m := range matchers {
		if m.Name == name {
			return true
		}
	}
	return false
}

func copyMatchersOnLabel(matchers []*labels.Matcher, name string) []*labels.Matcher {
	var out []*labels.Matcher
	for _, m := range matchers {
		if m.Name == name {
			copied := *m
			out = append(out, &copied)
		}
	}
	return out
}
