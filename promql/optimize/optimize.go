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

// Package optimize provides a small pipeline of optimization passes that
// rewrite a parsed PromQL expression into an equivalent, cheaper-to-evaluate
// expression before query execution begins.
//
// Passes in this package operate purely on parser.Expr trees and must not
// depend on anything beyond the query's own time range: no tenancy, no
// sharding, no remote execution. They run once, in a fixed order, before
// PreprocessExpr resolves durations and step-invariance, so a pass only ever
// sees the concrete node types the parser itself produces.
package optimize

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// ASTOptimizationPass rewrites a parsed PromQL expression into an equivalent
// expression that is cheaper to evaluate.
//
// Implementations must not retain the input Expr or any of its nodes beyond
// the call to Apply; the returned Expr must be safe for the caller to mutate
// and execute independently of the input. Apply must return an expression
// that is semantically equivalent to its input for every possible query
// result: an optimization pass changes how a query is evaluated, never what
// it evaluates to.
type ASTOptimizationPass interface {
	// Name returns a short, stable identifier for the pass, used in error
	// messages and for per-pass enablement.
	Name() string

	// Apply rewrites expr and returns the rewritten expression. start, end,
	// and step describe the time range the expression will be evaluated
	// over, matching the parameters PreprocessExpr already takes.
	Apply(ctx context.Context, expr parser.Expr, start, end time.Time, step time.Duration) (parser.Expr, error)
}

// Pipeline runs a fixed, ordered sequence of optimization passes.
//
// Each pass runs exactly once, in registration order; Pipeline does not
// iterate passes to a fixpoint. None of the passes registered by
// DefaultPipeline need fixpoint iteration today: add it only when a
// concrete pass requires re-running after a later pass changes the tree.
type Pipeline struct {
	passes []ASTOptimizationPass
}

// NewPipeline returns a Pipeline that runs passes in the given order.
func NewPipeline(passes ...ASTOptimizationPass) *Pipeline {
	return &Pipeline{passes: passes}
}

// Apply runs every pass in the pipeline, in order, feeding each pass's
// output into the next, and returns the fully rewritten expression.
//
// If p is nil or has no passes, Apply returns expr unchanged.
func (p *Pipeline) Apply(ctx context.Context, expr parser.Expr, start, end time.Time, step time.Duration) (parser.Expr, error) {
	if p == nil {
		return expr, nil
	}
	for _, pass := range p.passes {
		var err error
		expr, err = pass.Apply(ctx, expr, start, end, step)
		if err != nil {
			return nil, fmt.Errorf("optimize: pass %q: %w", pass.Name(), err)
		}
	}
	return expr, nil
}
