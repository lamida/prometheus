# Phase 4 design: is Subset Selector Elimination (SSE) worth building?

Status: design-discussion draft, not an accepted design. Like
`docs/query-planner-phase2-design.md` and `docs/query-planner-phase3-design.md`
before it, this exists to be argued with, not merged as a commitment to
build the feature it analyzes.

## Context

CSE (`promql/plan`'s `FromExpr`, `NormalizeForCSE`,
`CommonSubexpressionElimination`, and the Strategy A execution wiring in
`promql/engine.go`, gated behind
`EngineOpts.EnableCommonSubexpressionElimination`) only merges two subtrees
when `Node.EquivalentTo` says they are structurally identical — same
selector name, same matcher list (order-normalized), same offset, same `@`
timestamp, same histogram-bucket flag
(`promql/plan/nodes.go`'s `VectorSelectorNode.EquivalentTo`). Two selectors
that are related but not identical — e.g. `up{job="a"}` and
`up{job="a",env="prod"}`, where the second's result set is provably a
subset of the first's for any point in time — are two unrelated plan nodes
today, each issuing its own `storage.Querier.Select` call. This document
asks whether detecting and exploiting that subset relationship
("Subset Selector Elimination", SSE) is worth building, and if so, what it
would need to get right that CSE did not have to.

Unlike phase 3 (which concluded the feature under discussion was not worth
building, based on a benchmark), this document does not have a benchmark
yet — §6 proposes one as the concrete next step, the same way phase 3's §4
did before its own benchmark existed.

## 1. What SSE would detect

A relation weaker than `EquivalentTo`'s equality: node A *subsumes* node B
if, for every possible evaluation time, B's result set is guaranteed to be
a subset of A's. For `VectorSelectorNode`, a conservative, decidable
definition:

- Same `Name`, `Offset`, `Timestamp`, `SkipHistogramBuckets` as
  `EquivalentTo` already requires.
- A's `LabelMatchers` is a **matcher-set subset** of B's: every matcher A
  has, B also has (identical `labels.Matcher` value), and B has at least
  one additional matcher A lacks.

This is deliberately narrower than full semantic subsumption. Proving
`{job=~"x.*"}` subsumes `{job="foo"}` requires regex containment analysis,
which is undecidable in general for arbitrary REs and not attempted here.
v1 scope is exact-matcher-superset containment only: same matchers, same
matcher type (`=`, `!=`, `=~`, `!~` all compared by value, never
interpreted), plus strictly more matchers on the subsumed side. This misses
real cases (the regex example above) but never produces an incorrect
result, which matches CSE's own stated bias toward "never share when
uncertain" (see `hasUnstableOffset`'s doc in `nodes.go`).

## 2. Why this is not a CSE bucketing problem

`CommonSubexpressionElimination`'s algorithm (`cse.go`) is a single
bottom-up walk: group nodes into buckets by a cheap structural signature
(`cseSignature`), then do an `EquivalentTo` check within a bucket. That
works because equality is reflexive/symmetric/transitive — one pass,
one canonical survivor per group, done.

Subsumption is a **partial order**, not an equivalence relation: A can
subsume B, B can subsume C, and A doesn't subsume C directly unless
transitively checked; a selector can be subsumed by more than one other
selector with no single canonical "widest" one if two incomparable
supersets both exist (e.g. `{job="a"}` and `{env="prod"}` both subsume
`{job="a",env="prod"}`, but neither subsumes the other). `cseSignature`'s
bucketing (group by exact `Describe()` string) is useless here by
construction — two nodes in a subsumption relationship have different
`Describe()` output by definition (different matcher counts). SSE needs
its own pass, structured around picking, for each selector, the *cheapest
already-materialized superset available*, not a bucket-and-merge pass.
Where it plugs in: after `CommonSubexpressionElimination`, so it only has
to consider the already-deduplicated selector set (no point analyzing
subsumption between two nodes CSE has already proven identical).

## 3. Why this is a different execution problem, not just a different detection problem

This is the part phase 3 flagged as the hard question for any CSE-adjacent
feature, and it applies here more sharply than it did to `MultiAggregation`.

CSE's execution story (`materialize.go`'s occurrence-aliasing plus
`engine.go`'s `sharedNodeRefcount`/`cloneSharedValue`) works because a
shared node's two occurrences want the **identical value** —
`cloneSharedValue` (`engine.go:1362`) shallow-clones the outer `[]Series`
slice so both consumers see the same underlying point arrays, and that is
correct precisely because both consumers were going to compute the exact
same thing anyway.

SSE breaks that assumption on purpose: B's consumer wants a **filtered
subset** of A's series, not all of A's series. Sharing here cannot be
"point at the same value" — it has to be "derive my value from the shared
value by re-filtering it," which is a new kind of node, not a new
occurrence of an existing one:

- Fetch A's full match set once from storage (as today).
- Introduce something like `FilteredSelectorNode`, wrapping A's selector
  node plus B's residual matchers (the matchers B has that A doesn't).
- At evaluation time, once A's `Matrix`/series set is materialized, filter
  it in memory by checking each returned series' labels against B's
  residual matchers, rather than issuing B's own
  `storage.Querier.Select` call.

This needs new evaluator logic distinct from `cloneSharedValue`'s
pointer-aliasing path — a filter-and-project step over an already-realized
value, which `promql/engine.go` has no equivalent of today for this
purpose. It is a materially larger execution-side change than CSE's
Strategy A wiring was, which only needed a cache-and-clone, not a
filter-and-derive.

## 4. Where the win is, and where this makes things worse

The win only exists when B is more expensive to select directly than
filtering A's already-fetched result in memory would be — e.g. A and B
share the same high-cardinality `__name__`, A's match set is modest, and B
would otherwise force its own separate index walk plus chunk
decompression for what turns out to be a large overlapping set of series.

The failure mode is real and needs guarding against, not just noting: if
A's match set is *much* larger than what B's own direct selection would
have cost, this pass makes the query worse — it forces materializing all
of A into memory (every series, every sample in range) just to serve a
narrow filter for B, when B's own direct `Select` alone would have been
cheaper. Nothing in Prometheus's engine today estimates selector
cardinality before running a query, so there is no existing signal to
decide "is A cheap enough to fetch in full" without guessing. A v1
implementation would need either:

- A static heuristic (e.g. only share when A and B have the same `Name`,
  on the theory that same-metric selectors are more likely to have
  comparable cardinality than arbitrary same-shape-different-metric
  pairs), or
- Skip the decision entirely and always share, accepting some fraction of
  regressed queries, which does not match this project's stated
  bias — phase 2 and phase 3 both treated "prove it doesn't regress common
  cases" as a precondition to building, not an acceptable cost.

This is the single open risk that most needs resolving — probably via the
same benchmark-before-building discipline phase 3 used — before any code
gets written, since unlike CSE (which strictly reduces work: one
computation instead of two identical ones), SSE can strictly *increase*
work in the wrong shape of query.

## 5. Interaction with already-landed Tier 1 passes

`docs/query-planner-analysis.md` already flags `ReduceMatchers` and
`PropagateMatchers` as Tier 1, portable, real-value passes (and
`PropagateMatchers` has since landed —
`promql/optimize`, PR #6). Both already reduce or share matchers *before*
SSE would run. Worth checking, the same way phase 3 checked whether
`MultiAggregation` had anything left to capture once CSE already ran,
whether `PropagateMatchers` already eliminates most of the cases SSE would
otherwise catch (e.g. a binary expression's `on`/`ignoring` propagation may
already turn what would have been a subsumption case into an equality case
CSE handles today) — if so, SSE's remaining incremental value could turn
out as thin as `MultiAggregation`'s did in phase 3.

## 6. Proposed next step: measure before building

Following phase 3's §4 pattern exactly: do not commit to a
`FilteredSelectorNode` design before measuring whether the win this
document argues for is real and where it stops being real.

Concretely: a `promql/subsetselector_bench_test.go`-style benchmark
comparing, for a representative set of selector pairs (A matcher count vs.
B's residual matcher count, varying A's series cardinality and B's
resulting subset cardinality as a fraction of A's):

- `unshared`: two independent selections, A and B each running its own
  `storage.Querier.Select` (today's behavior).
- `shared_prototype`: a hand-rolled (non-plan-integrated) version that
  selects A once, then filters B's residual matchers against A's result
  set in memory.

The specific numbers to look for: at what ratio of "B's subset size ÷ A's
full size" does `shared_prototype` stop beating `unshared`, and how
sensitive that crossover point is to A's absolute cardinality. That
crossover point is the concrete evidence needed to answer §4's heuristic
question, rather than guessing at a static rule.

## Recommendation

Do not build `FilteredSelectorNode` or wire subsumption-based sharing into
`promql/engine.go` yet. Unlike CSE (a strict, unconditional win: identical
work computed once instead of twice) and unlike `MultiAggregation`
(marginal, but never negative), SSE is a pass that can regress query
performance for shapes it misjudges, and there is currently no cardinality
signal in Prometheus's engine to make that judgment reliably. §6's
benchmark is the prerequisite for finding out whether a heuristic exists
that makes the win reliably positive, and for characterizing whether
`PropagateMatchers` (§5) already captures most of the available benefit
without this pass at all.

## Open questions

1. Is v1 scope (exact-matcher-superset containment, no regex reasoning)
   worth building at all, or does §5's overlap with `PropagateMatchers`
   already close most of the gap it would fill?
2. Does §6's benchmark find a heuristic (static, based on selector shape
   alone) that reliably predicts when sharing helps vs. regresses, or does
   this fundamentally require a cardinality estimate Prometheus's engine
   does not compute today?
3. Should this extend to `MatrixSelectorNode`/range selectors, or is
   vector-selector-only a reasonable v1 boundary (mirroring how CSE itself
   started narrower, e.g. `hasUnstableOffset`'s conservative
   never-share rule)?
4. Does subsumption need the same `hasUnstableOffset` conservatism CSE
   applies (never share across a subquery/step-invariant boundary), and if
   so, is that guard identical to CSE's existing one or does it need its
   own?
