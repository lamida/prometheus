# Phase 3 design: is `MultiAggregation` worth porting to Prometheus?

Status: design-discussion draft, not an accepted design. Like
`docs/query-planner-phase2-design.md` before it, this exists to be argued
with, not merged as a commitment to build the feature it analyzes.

## Context

`docs/query-planner-phase2-design.md` §6 deferred `MultiAggregation` out of
the CSE work that has since landed (`promql/plan`'s `FromExpr`,
`EliminateDeduplicateAndMerge`, `CommonSubexpressionElimination`, and the
Strategy A execution wiring in `promql/engine.go`, gated behind
`EngineOpts.EnableCommonSubexpressionElimination`), on the grounds that it
"needs its own execution-strategy question answered — a multi-output
aggregation node producing more than one Matrix from a single node is a new
evaluator capability... and should be its own design pass and PR series
once CSE has landed and proven the DAG/refcounting machinery out, not
bundled with CSE itself." CSE has now landed and proven out (four PRs, a
differential test suite, the full `promqltest` builtin corpus, and the
existing `promql` package suite all passing under `-race`). This document is
that follow-up design pass §6 called for.

Its conclusion is different in kind from phase 2's: phase 2 proposed a
concrete design and asked for it to be pressure-tested. This document instead
asks whether `MultiAggregation` is worth building **at all**, given what CSE
already delivers in Prometheus's execution model, and concludes — with a
specific, checkable technical argument, not a guess — that it is not, absent
evidence from a benchmark that hasn't been run yet. Section 4 proposes that
benchmark as the actual next step, in place of building the feature blind.

## 1. What `MultiAggregation` does in Mimir, precisely

Mimir's `MultiAggregation` (`pkg/streamingpromql/optimize/plan/multiaggregation`)
fires when its CSE pass has already produced a `Duplicate` node (Mimir's
equivalent of a plan node with `ParentCount() > 1`) with two or more
`AggregateExpression` parents — e.g. `sum(x) + avg(x)` where `x` has already
been shared by CSE. Rather than leaving each `AggregateExpression` to pull
its own copy of `x`'s output series from the shared `Duplicate` node one
series at a time, it replaces the whole group with one `MultiAggregationGroup`
(wrapping the shared input once) and one `MultiAggregationInstance` per
original aggregation, each configured with its own operator
(`sum`/`avg`/`min`/...), grouping, and (from plan version 8 on) an optional
label-matcher filter restricting which subset of the shared input's series it
aggregates.

The actual fusion happens in `MultiAggregatorGroupEvaluator.ReadNextSeries`
(`operator.go`): it pulls exactly one series at a time from the shared inner
operator, and for that one series, calls `AccumulateNextInnerSeries` on every
instance that still needs it — so a series that both `sum` and `avg` want to
aggregate is read from the inner operator's stream **once**, not twice, and
handed to both accumulators before the next series is pulled. This is a
genuine, distinct optimization on top of CSE's own "don't recompute the
shared subexpression" benefit: even after CSE ensures `x` is computed once,
Mimir's streaming execution model would otherwise still require each of
`sum`'s and `avg`'s per-series pull cycles to independently re-pull that
already-computed series data from the shared node's stream, which is real,
measurable per-series overhead in a pull-based streaming engine.

## 2. Why that specific benefit mostly does not exist in Prometheus's execution model

Prometheus's evaluator is not pull-based/streaming; it materializes a whole
`Matrix` (every series, every point, for the query's full time range) in one
call and hands that same Go value to the caller. This difference in
execution model is exactly what `docs/query-planner-phase2-design.md`'s §4
already had to grapple with for CSE itself (Strategy A's refcounted pooling
exists **because** Prometheus has no per-series streaming buffer to reach
for) — and it matters again here, in the opposite direction: a cost Mimir's
model pays repeatedly (re-pulling an already-computed series once per
consumer) is a cost Prometheus's model does not pay at all once CSE's
existing Strategy A wiring is in place.

Trace the actual call path for `sum(x) + avg(x)` once CSE has already merged
the two `x` occurrences into one shared `parser.Expr` node (this already
happens today, unconditionally, whenever
`EngineOpts.EnableCommonSubexpressionElimination` is set and the two
occurrences are structurally equivalent and eligible — see
`promql/plan/materialize.go`):

- `sum(x)`'s `*parser.AggregateExpr` case in `eval` (`promql/engine.go`,
  around the `case *parser.AggregateExpr:` block) calls
  `ev.eval(ctx, e.Expr)` to get `x`'s `Matrix`, then calls
  `ev.rangeEvalAgg(ctx, e, ...)` over it.
- `avg(x)`'s own `*parser.AggregateExpr` case does the same: calls
  `ev.eval(ctx, e.Expr)` with the exact same (materialized, aliased)
  `x` pointer.
- Because `x` is a tracked key in `ev.sharedNodeRefcount` (Strategy A, landed
  in the CSE execution-wiring PR), `avg`'s call to `ev.eval` is a cache hit:
  it gets `cloneSharedValue(cached.val)` — a shallow clone of the outer
  `[]Series` slice, with every `Series.Floats`/`Histograms` pointing at the
  *same* underlying arrays `sum`'s call already computed — instead of
  recomputing `x`'s selection/rate/whatever-it-is expression tree at all.

So the one thing Mimir's `MultiAggregation` exists to avoid — a consumer of
a shared node paying its own cost to obtain that node's already-computed
data — is **already avoided**, today, by the CSE work already merged, for
the dominant cost in the overwhelming majority of real queries: actually
producing `x`'s series (selecting from storage, evaluating `rate()` or
whatever function wraps the selector, evaluating nested binary expressions,
and so on). Mimir's own production number cited in
`docs/query-planner-analysis.md` — "~50% lower memory on ruler-query-frontend
workloads" — is attributed there to `CommonSubexpressionElimination`, not to
`MultiAggregation` layered on top of it; nothing in this analysis or that
document's own numbers suggests `MultiAggregation` is where the bulk of
Mimir's win comes from either.

## 3. What actually remains un-deduplicated after CSE, and how small it plausibly is

`sum`'s and `avg`'s `rangeEvalAgg` calls are still two separate calls, each
doing its own:

- Per-step gathering of the (already-shared, already-cheap-to-obtain) input
  vector (`ev.gatherVector`).
- Grouping-key computation for its own `by`/`without` clause (which may
  differ between the two aggregations — nothing requires `sum(x)` and
  `avg(x)` to group the same way, and Mimir's own design does not assume
  they do: each `MultiAggregationInstance` configures its own independent
  `grouping`/`without`).
- The actual incremental aggregation arithmetic (`ev.aggregation`,
  `promql/engine.go`'s per-group accumulation).

This is real, non-zero work, and it is duplicated once per aggregation
sharing the input today. But it is a fundamentally cheaper category of work
than what CSE already deduplicates: it is O(series × steps) simple
arithmetic and hashing over data that is already in memory, not I/O,
decompression, or the kind of nested-function evaluation (`rate`,
`histogram_quantile`, deeply nested binary expressions, and so on) that CSE's
existing win targets. Whether it is *worth* building new evaluator machinery
to deduplicate is an empirical question this document cannot answer from
first principles — which is precisely why §4 proposes measuring it before
writing any more evaluator-internals code, rather than repeating phase 2's
pattern of designing a concrete mechanism before the case for it is
established.

There is also a structural reason to expect the win, if any, to be modest:
unlike Mimir's `MultiAggregationGroup`/`Instance` split — which exists to
solve a *streaming* problem (don't re-pull a series from a live pull-based
operator chain) — a Prometheus-side fused-aggregation node would need to
solve a *batch* problem (don't redundantly re-scan an already-fully-resident
`Matrix` once per aggregation), which is the kind of thing a well-written
single-pass loop already does cheaply in Go, without needing a new operator
abstraction, new plan node types, `ParentCount()`-keyed detection logic, or
changes to `engine.go`'s pooling/memoization machinery a second time. If the
win is real, capturing it well might look more like "one rangeEval-style
helper that computes N groupings from one input scan" than a full port of
Mimir's `Group`/`Instance` architecture — a much smaller, lower-risk change
than phase 2's Strategy A was, *if* it turns out to be worth doing at all.

## 4. Proposed next step: measure before building

Rather than proposing a concrete `MultiAggregation` node design (the thing
phase 2 did, and the thing §6 originally called for as this document's
job), this document proposes a smaller, safer first step: **a benchmark**
that isolates and measures exactly the cost this document argues is small,
so the decision to build (or not build) a fused-aggregation mechanism is
made from a number, not a plausibility argument.

Concretely: `promql/bench_test.go`-style benchmarks (or a new
`promql/multiaggregation_bench_test.go`) comparing, for a representative set
of queries of the shape `agg1(x) <op> agg2(x) <op> agg3(x)` (varying: number
of aggregations sharing one input, series cardinality, step count, whether
the aggregations share a grouping or not):

- Wall-clock and allocation cost with `EnableCommonSubexpressionElimination`
  on today (CSE dedupes `x` itself; each aggregation still runs its own
  `rangeEvalAgg`).
- The same queries' CSE-attributable cost breakdown — e.g. via CPU profiling
  (`go test -cpuprofile`) — to see what fraction of total query time, post-CSE,
  is actually spent inside `rangeEvalAgg`/`ev.aggregation` versus inside
  producing the shared input `x` in the first place.

If that breakdown shows `rangeEvalAgg`'s own share of total time is a small
fraction of the post-CSE total across realistic queries (this document's
prediction, not yet a measured fact), that is direct evidence
`MultiAggregation`'s marginal value on top of already-merged CSE is small in
Prometheus specifically, and building the feature is not justified by this
analysis. If the breakdown instead shows `rangeEvalAgg` dominating for some
realistic, common query shape (e.g. very high cardinality with many
aggregations sharing one input, where the O(series × aggregations) grouping
cost could plausibly dominate over an already-cheap, already-shared input),
that would be the concrete, numeric justification a phase-3-and-a-half
design doc proposing an actual node design should open with — something
this document deliberately does not attempt, for lack of that evidence.

## Recommendation

**Do not build a `MultiAggregation`-equivalent plan pass or evaluator
capability in Prometheus at this time.** The specific inefficiency it exists
to solve in Mimir — redundant per-series re-pulls from a shared streaming
operator — has no direct analogue in Prometheus's batch-materialization
execution model once CSE's already-merged Strategy A wiring is in place;
what (small, plausibly) remains is a different, cheaper category of
duplicated work (per-aggregation grouping/accumulation over an
already-shared, already-resident `Matrix`), not the thing Mimir's design
targets. Building new plan node types and evaluator-internals machinery
carries real review/maintenance/correctness cost — exactly the kind of cost
phase 2's Strategy A work incurred and was worth incurring, because CSE's
win (avoiding recomputation of expensive nested subexpressions) is large and
verified by Mimir's own production numbers. This document does not have an
equivalent number for `MultiAggregation` specifically in Prometheus's model,
and the structural argument in §2-§3 suggests one may not exist. §4's
benchmark is the concrete, cheap, low-risk next step that would either
produce that number or confirm there isn't one to chase.

## Open questions

1. Is there a realistic, common query shape (high cardinality, many
   aggregations sharing one input, mismatched groupings) where
   `rangeEvalAgg`'s own cost genuinely dominates post-CSE total query time?
   §4's benchmark is how to find out; this document does not know the answer.
2. If §4's benchmark does find a dominating case, is the right fix a
   Mimir-style `MultiAggregation` port, or a narrower, Prometheus-specific
   optimization (e.g. sharing grouping-key computation across aggregations
   that happen to use the same `by`/`without` clause, without a general
   multi-output node abstraction)? This document takes no position, since it
   concludes the broader question ("is this worth building at all") likely
   resolves to "no" before this narrower one needs answering.
3. Does Mimir's own production experience have a breakdown of
   `MultiAggregation`'s standalone contribution (separate from CSE's) to its
   cited memory/latency wins? If Mimir's own numbers already show
   `MultiAggregation` contributing negligibly on top of CSE even in its own
   streaming model, that would be independent corroboration for this
   document's argument that it's not worth porting; this document was not
   able to find such a breakdown in the code or comments available locally
   and does not claim one exists either way.
