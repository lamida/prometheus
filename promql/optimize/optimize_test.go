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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/parser"
)

// recordingPass is a minimal ASTOptimizationPass that records that it ran
// (by appending its name to a shared log) and can optionally return an
// error, so tests can observe Pipeline.Apply's ordering and error-wrapping
// behavior without depending on any real pass's rewrite logic.
type recordingPass struct {
	name string
	log  *[]string
	err  error
}

func (p recordingPass) Name() string { return p.name }

func (p recordingPass) Apply(_ context.Context, expr parser.Expr, _, _ time.Time, _ time.Duration) (parser.Expr, error) {
	*p.log = append(*p.log, p.name)
	if p.err != nil {
		return nil, p.err
	}
	return expr, nil
}

func mustParse(t *testing.T, query string) parser.Expr {
	t.Helper()
	expr, err := testParser.ParseExpr(query)
	require.NoError(t, err)
	return expr
}

// TestPipeline_NilPipelineIsNoOp verifies Apply's documented nil-receiver
// behavior: a nil *Pipeline returns its input expression unchanged, with no
// error, rather than panicking.
func TestPipeline_NilPipelineIsNoOp(t *testing.T) {
	var p *Pipeline
	expr := mustParse(t, `foo{job="a"}`)

	got, err := p.Apply(context.Background(), expr, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)
	require.Same(t, expr, got, "expected a nil Pipeline to return the input expression unchanged")
}

// TestPipeline_EmptyPipelineIsNoOp verifies that a Pipeline constructed with
// no passes at all also returns its input unchanged.
func TestPipeline_EmptyPipelineIsNoOp(t *testing.T) {
	p := NewPipeline()
	expr := mustParse(t, `foo{job="a"}`)

	got, err := p.Apply(context.Background(), expr, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)
	require.Same(t, expr, got)
}

// TestPipeline_RunsPassesInRegistrationOrder verifies Apply runs every
// registered pass exactly once, in the order they were passed to
// NewPipeline, feeding each pass's own output into the next.
func TestPipeline_RunsPassesInRegistrationOrder(t *testing.T) {
	var log []string
	p := NewPipeline(
		recordingPass{name: "first", log: &log},
		recordingPass{name: "second", log: &log},
		recordingPass{name: "third", log: &log},
	)

	expr := mustParse(t, `foo{job="a"}`)
	_, err := p.Apply(context.Background(), expr, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)

	require.Equal(t, []string{"first", "second", "third"}, log, "expected every pass to run exactly once, in registration order")
}

// TestPipeline_ApplyWrapsPassError verifies that when a pass fails, Apply
// stops running any further passes and returns an error that identifies
// which pass failed (by name) and wraps the pass's own error, per Apply's
// doc comment.
func TestPipeline_ApplyWrapsPassError(t *testing.T) {
	var log []string
	innerErr := errors.New("boom")
	p := NewPipeline(
		recordingPass{name: "first", log: &log},
		recordingPass{name: "failing", log: &log, err: innerErr},
		recordingPass{name: "never-runs", log: &log},
	)

	expr := mustParse(t, `foo{job="a"}`)
	got, err := p.Apply(context.Background(), expr, time.Time{}, time.Time{}, 0)

	require.Nil(t, got, "expected Apply to return a nil expression on pass failure")
	require.Error(t, err)
	require.ErrorIs(t, err, innerErr, "expected Apply's returned error to wrap the failing pass's own error")
	require.Contains(t, err.Error(), "failing", "expected Apply's error to identify which pass failed by name")
	require.Equal(t, []string{"first", "failing"}, log, "expected Apply to stop running passes after the first failure")
}
