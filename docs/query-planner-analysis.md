# Could Prometheus's PromQL engine benefit from a query planner?

Status: analysis / discussion draft, not a design doc for an accepted feature.

## Summary

Prometheus's PromQL engine (`promql/engine.go`) has no plan layer: it parses a
query into an AST and walks it directly (`evaluator.eval`), with the single
exception of `PreprocessExpr`, a narrow rewrite for the `@` modifier and
step-invariant expressions. Mimir's PromQL engine (MQE,
`pkg/streamingpromql/`) has spent about three years building an optimizing
planner with a dozen-plus rewrite passes.

This document checks, pass by pass, which of those passes are actually about
optimizing a query on a single `storage.Queryable`, and which exist to solve
problems specific to Mimir being a distributed, multi-tenant system. Only the
former are relevant to Prometheus, which runs as a single binary against its
own local TSDB and has no tenant concept. The filter applied to every pass is
one question: does this need anything beyond a plain `storage.Queryable`, or
does it depend on tenancy, sharding, or talking to another process?

Findings, from `github.com/grafana/mimir` (`pkg/streamingpromql/optimize/`,
`pkg/streamingpromql/planning.go`) as of 2026-07-09:

- **7 passes are portable with real, attributable value** — no dependency on
  `dskit/tenant`, sharding, or the frontend/querier RPC boundary.
- **6 passes are portable but marginal** — no tenant/remote dependency, but
  negligible savings, already redundant, or already handled natively by
  Prometheus's engine.
- **5 passes/mechanisms are not portable** — they exist specifically to solve
  multi-tenant fan-out, frontend result caching, or cross-process execution,
  none of which apply to a single-node, single-tenant Prometheus.

## Method

For each optimization mechanism registered through
`ASTOptimizationPass`/plan-level pass interfaces in
`pkg/streamingpromql/optimize/` (both the querier-side `QuerierQueryPlanner`
and the frontend-only `QueryFrontendQueryPlanner` in
`pkg/streamingpromql/planning.go`), plus the legacy AST "Mapper" rewrites in
`pkg/frontend/querymiddleware/rewrite.go` that live in the same package but
predate the plan-based interface: checked its imports and implementation for
any dependency on `dskit/tenant`, sharding, the frontend result cache, or
`frontend/v2/remoteexec.go`.

Notably, even inside Mimir the tenant/distribution-aware passes are kept
structurally separate: `QuerierQueryPlanner` (registered on every querier) is
a strict subset of `QueryFrontendQueryPlanner` (registered only on the
frontend, which adds subquery spin-off and sharding). That separation inside
Mimir's own architecture is itself evidence that the optimizer and the
distributed-fan-out logic are two different concerns that happen to share a
codebase.

## Tier 1 — portable, real value, worth upstreaming discussion

No dependency on `dskit/tenant`, sharding, or remote execution in any of
these. Each would plug into Prometheus's engine against nothing more than the
`storage.Queryable` it already has.

| Pass | What it does | Prometheus analog needed |
|---|---|---|
| `CommonSubexpressionElimination` | Computes a repeated subexpression once and reuses it (e.g. `sum(a) / (sum(a) + sum(b))`) | A plan layer with node identity/structural comparison; no cross-subtree caching exists in `promql/engine.go` today |
| `ReduceMatchers` | Drops redundant/duplicate matchers on a selector before it hits the index | Cheaper `tsdb/querier.go` index lookups; purely static AST rewrite |
| `PropagateMatchers` | Copies matchers across a binary operation via `on`/`ignoring` so both sides fetch less | Static AST rewrite; legacy "Mapper" style, not yet plan-based even in Mimir |
| `NarrowBinarySelectors` | Evaluates one side of a binary op, then uses the actual label values returned to narrow what gets fetched for the other side | Runtime technique, but needs no sharding — works identically against one local `storage.Queryable` |
| `RemoveStaticallyEmptyExpressions` | Proves a subtree can never return data and skips it entirely | Dead-branch elimination Prometheus's tree-walking evaluator doesn't do today |
| `MultiAggregation` | Fuses multiple aggregations over the same input into a single pass over the data | Builds on CSE having identified the shared input first |
| `EliminateDeduplicateAndMerge` | Removes redundant plan nodes | Requires a plan representation to have redundant nodes in the first place |

Production numbers already measured on Mimir for two of these (real, not
simulated — kept here for context, not as a claim that Prometheus would see
identical numbers):

- `NarrowBinarySelectors`: a query went from >1 min / 24h range / 85 MB
  fetched to <4s / 30-day range / 4 MB fetched.
- `CommonSubexpressionElimination`: ~50% lower memory on ruler-query-frontend
  workloads (mimir-prod-36, July 2025).

## Tier 2 — portable, not worth leading with

No tenant/remote dependency, but low or already-realized value:

| Pass | Why it's Tier 2 |
|---|---|
| `CollapseConstants` | Folds `2 + 3` → `5` at plan time; such expressions are rare and cheap regardless |
| `PruneToggles` | Strips toggled-off branches; generic, low impact |
| `SortLabelsAndMatchers` | Only pays off as infrastructure for other passes, not standalone |
| `ReorderHistogramAggregation` | Never independently evaluated for standalone value |
| `InsertOmittedTargetInfoSelector` | Portable in principle, but payoff depends on Prometheus's own OTel-ingestion support |
| `SkipHistogramDecoding` | Moot — Prometheus's engine already does this natively in `promql/engine.go` |

## Tier 3 — not portable, need something single-node Prometheus lacks

| Pass / mechanism | Why it fails the test |
|---|---|
| `Sharding` (`optimize/ast/sharding`) | Imports `github.com/grafana/dskit/tenant`, calls `tenant.TenantIDs(ctx)` directly; rewrites the query to fan out across per-tenant shards. Needs multiple tenants/shards to have a job to do. |
| `SubqueryStyleSpinoff` | Exists solely to prepare subqueries for sharding — no purpose without `Sharding` |
| `RangeVectorSplitting` | Splits a range-vector fetch into cacheable sub-ranges, feeding a frontend result cache Prometheus has no equivalent of |
| `SplitAndCache` | Range-query splitting + result caching inside MQE; a frontend-caching concept with no single-node analog |
| Remote execution (`frontend/v2/remoteexec.go`, `RemoteExecutionGroup`/`Consumer`) | Inserted at the frontend/querier process boundary; meaningless without a querier/frontend process split |

For context: projection pushdown was implemented and later removed from
Mimir ([mimir#15618](https://github.com/grafana/mimir/pull/15618)) for
cost/complexity reasons (a TSDB head/block-building conflict), not
correctness — kept here only as a "this existed and was cut" data point, not
a portability example either way.

## What this does and doesn't mean for Prometheus

Prometheus already has a pluggable engine interface (`promql.QueryEngine`,
`engine.go:126`), consumed generically by `web/api/v1/api.go` and
`rules/manager.go`. Swapping in an alternative engine is already possible
without forking Prometheus, so this analysis isn't proposing a new extension
point. It's answering a narrower question: what would Prometheus's *own*
reference engine gain from adopting a plan layer at all.

The honest answer, based on the above: a real but bounded win. Seven passes
have no distributed-systems dependency and, in principle, no reason not to
work identically inside `promql/engine.go`. That's most of Mimir's pass
inventory, not a lucky handful. But building even that Tier 1 subset into
Prometheus would still require:

1. Introducing a plan representation and an AST/plan boundary where none
   exists today — the tree-walking `evaluator.eval` interpreter would need to
   become a two-stage parse → optimize → execute pipeline, which is a
   nontrivial engine change on its own, independent of any individual pass.
2. Re-validating each pass's rewrite equivalence against Prometheus's own
   evaluator semantics rather than assuming Mimir's correctness work transfers
   for free (native histograms, staleness handling, and lookback-delta
   semantics all differ in the details between the two evaluators).
3. Benchmarking each pass against Prometheus's own workloads — Mimir's
   production numbers above are real but were measured against Mimir's
   query-frontend and multi-tenant traffic mix, not standalone Prometheus
   query patterns.

None of that is disqualifying, but it means "port the optimizer" is a
multi-pass engine project, not a drop-in dependency, even after subtracting
everything that's genuinely Mimir-specific.

## Sources

- `github.com/grafana/mimir`, `pkg/streamingpromql/optimize/`,
  `pkg/streamingpromql/planning.go`, `pkg/frontend/querymiddleware/rewrite.go`,
  `pkg/mimir/modules.go` (main, checked 2026-07-09).
- `github.com/prometheus/prometheus`, `promql/engine.go` (main, checked
  2026-07-09).
