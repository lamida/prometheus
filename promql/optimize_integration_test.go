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

package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
)

// TestEngineOptimizationPasses verifies that enabling EngineOpts.EnableOptimizationPasses
// runs the promql/optimize pipeline (currently just ReduceMatchers) over a query before
// evaluation, and that doing so does not change the query's result: the option changes how
// a query is evaluated, never what it evaluates to.
func TestEngineOptimizationPasses(t *testing.T) {
	const loadStmts = `
load 30s
	http_requests_total{job="api-server", instance="0", group="production"}	0+10x100
	http_requests_total{job="api-server", instance="1", group="production"}	0+20x100
`
	queries := []string{
		`http_requests_total{job="api-server", job="api-server"}`,
		`rate(http_requests_total{job="api-server", job="api-server"}[5m])`,
		`http_requests_total{job="api-server"}`, // No duplicates: pipeline should be a no-op.
	}

	newEngine := func(t *testing.T, enableOptimizationPasses bool) *promql.Engine {
		return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
			MaxSamples:               promqltest.DefaultMaxSamplesPerQuery,
			Timeout:                  100 * time.Second,
			EnableOptimizationPasses: enableOptimizationPasses,
			Parser:                   parser.NewParser(promqltest.TestParserOpts),
		})
	}

	at := time.Unix(600, 0)
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			storage := promqltest.LoadedStorage(t, loadStmts)
			defer storage.Close()

			withoutPasses := newEngine(t, false)
			withPasses := newEngine(t, true)

			run := func(engine *promql.Engine) *promql.Result {
				qry, err := engine.NewInstantQuery(context.Background(), storage, nil, q, at)
				require.NoError(t, err)
				defer qry.Close()
				return qry.Exec(context.Background())
			}

			want := run(withoutPasses)
			require.NoError(t, want.Err)
			got := run(withPasses)
			require.NoError(t, got.Err)
			require.Equal(t, want.Value, got.Value)
		})
	}
}

// TestEngineOptimizationPassesDisabledByDefault verifies EnableOptimizationPasses defaults
// to false, so an Engine constructed without explicitly setting it never runs the
// promql/optimize pipeline.
func TestEngineOptimizationPassesDisabledByDefault(t *testing.T) {
	engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: promqltest.DefaultMaxSamplesPerQuery,
		Timeout:    100 * time.Second,
		Parser:     parser.NewParser(promqltest.TestParserOpts),
	})
	storage := promqltest.LoadedStorage(t, `
load 30s
	http_requests_total{job="api-server", instance="0"}	0+10x100
`)
	defer storage.Close()

	qry, err := engine.NewInstantQuery(context.Background(), storage, nil, `http_requests_total{job="api-server", job="api-server"}`, time.Unix(600, 0))
	require.NoError(t, err)
	defer qry.Close()
	res := qry.Exec(context.Background())
	require.NoError(t, res.Err)
}
