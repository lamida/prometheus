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

// This file is package optimize_test, not optimize, because it imports
// promql to run queries end to end. promql itself imports promql/optimize
// (to build its default optimization pipeline), so an internal test file
// here that also imported promql would make the optimize package's test
// binary depend on itself through promql, an import cycle Go test builds
// reject. The external test package avoids that: it only depends on
// optimize's exported API, exactly like any other caller of this package.
package optimize_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/optimize"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
)

// assertPassPreservesSemantics runs query and pass.Apply(query)'s rewritten
// form through the same engine and storage fixture, at the same evaluation
// time, and asserts they produce identical results. This is the strongest
// regression net for "this optimization changed how a query is evaluated,
// not what it evaluates to" available without wiring the pass into Engine
// itself: it exercises the pass exactly as a caller would (parse, rewrite,
// re-render to a query string) against Prometheus's real, unmodified query
// engine.
func assertPassPreservesSemantics(t *testing.T, pass optimize.ASTOptimizationPass, loadStmts, query string) {
	t.Helper()

	storage := promqltest.LoadedStorage(t, loadStmts)
	defer storage.Close()
	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)

	original, err := parser.NewParser(promqltest.TestParserOpts).ParseExpr(query)
	require.NoError(t, err)

	at := time.Unix(600, 0)
	rewritten, err := pass.Apply(context.Background(), original, at, at, 0)
	require.NoError(t, err)

	runAt := func(qs string) *promql.Result {
		qry, err := engine.NewInstantQuery(context.Background(), storage, nil, qs, at)
		require.NoError(t, err)
		defer qry.Close()
		return qry.Exec(context.Background())
	}

	want := runAt(query)
	require.NoError(t, want.Err)
	got := runAt(rewritten.String())
	require.NoError(t, got.Err)
	require.Equal(t, want.Value, got.Value)
}

func TestReduceMatchers_PreservesSemantics(t *testing.T) {
	const loadStmts = `
load 30s
	http_requests_total{job="api-server", instance="0", group="production"}	0+10x100
	http_requests_total{job="api-server", instance="1", group="production"}	0+20x100
	http_requests_total{job="api-server", instance="0", group="canary"}	0+30x100
`
	queries := []string{
		`http_requests_total{job="api-server", job="api-server"}`,
		`rate(http_requests_total{job="api-server", job="api-server"}[5m])`,
		`sum(http_requests_total{group="production", group="production"}) by (job)`,
		`http_requests_total{job="api-server", job="api-server"} / http_requests_total{group="canary", group="canary"}`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			assertPassPreservesSemantics(t, optimize.ReduceMatchers{}, loadStmts, q)
		})
	}
}

func TestPropagateMatchers_PreservesSemantics(t *testing.T) {
	const loadStmts = `
load 30s
	up{job="a", instance="0"}	1x100
	up{job="b", instance="0"}	1x100
	down{job="a", instance="0"}	1x100
	down{job="b", instance="0"}	1x100
`
	queries := []string{
		`up{job="a"} == on(job) down`,
		`up == on(job) down{job="a"}`,
		`up{job="a"} == on(job) down{job="b"}`,
		`up == on(job) down`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			assertPassPreservesSemantics(t, optimize.PropagateMatchers{}, loadStmts, q)
		})
	}
}
