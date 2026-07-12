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

## §6 results: the benchmark, run

`promql/subsetselector_bench_test.go` (`BenchmarkSubsetSelectorCandidate`)
implements §6's proposal directly, operating on a `storage.Querier` (there is
no `FilteredSelectorNode` to integrate into a plan; this is the hand-rolled
prototype §6 asks for). For A = `{__name__="sse_test", job="x"}` and B = A
plus one residual matcher, at three cardinalities of A (100, 1,000, 10,000
series) and three ratios of B's cardinality to A's (0.50, 0.10, 0.01), it
compares `unshared` (independent `Select(A)` and `Select(B)`, each fully
decoded) against `shared_prototype` (`Select(A)` once, fully decoded, then B
derived by checking the residual matcher against A's already-decoded series
in memory).

Measured on this machine (Apple M4 Pro, `-benchtime=20x -count=3`, median of
the three; absolute numbers will vary by hardware, the crossover behavior
between columns is what matters):

| \|A\| | ratio | unshared | shared\_prototype | shared ÷ unshared |
|---|---|---|---|---|
| 100    | 0.50 | 0.369ms | 0.251ms |  0.68× |
| 100    | 0.10 | 0.214ms | 0.216ms |  1.01× |
| 100    | 0.01 | 0.191ms | 0.207ms |  1.08× |
| 1,000  | 0.50 | 2.930ms | 2.283ms |  0.78× |
| 1,000  | 0.10 | 2.064ms | 2.222ms |  1.08× |
| 1,000  | 0.01 | 1.908ms | 2.250ms |  1.18× |
| 10,000 | 0.50 | 29.36ms | 23.08ms |  0.79× |
| 10,000 | 0.10 | 21.23ms | 23.09ms |  1.09× |
| 10,000 | 0.01 | 19.45ms | 23.02ms |  1.18× |

Two things fall out of this, both confirming §4's concern rather than
resolving it in SSE's favor:

1. **`shared_prototype`'s cost is nearly flat across ratio, at fixed
   `|A|`.** 2.22-2.33ms across all three ratios at `|A|=1,000`,
   23.0-23.1ms at `|A|=10,000`. This is exactly what §3 predicts: the cost
   is dominated by fully materializing A (index walk plus chunk decode for
   every one of A's series), which does not shrink no matter how selective
   B's residual matcher is. The in-memory filter step itself is cheap
   enough not to show up.
2. **`unshared`'s cost scales down with ratio, and crosses under
   `shared_prototype` between ratio 0.5 and 0.1 at every cardinality
   tested.** Interpolating linearly between the ratio=0.5 and ratio=0.1
   measurements for where `unshared` equals `shared_prototype`'s
   (roughly-constant) cost gives a crossover ratio of ~0.12 (`|A|=100`),
   ~0.21 (`|A|=1,000`), ~0.19 (`|A|=10,000`). Below that ratio, B is cheap
   enough to select directly that materializing all of A to serve it is a
   net loss; above it, sharing wins.

This reproduces §4's stated failure mode empirically, not just
structurally: sharing is a win only when B is not much more selective than
A (roughly ratio ≳ 0.2 in this fixture), and a regression otherwise. The
crossover ratio is reasonably stable across a 100× cardinality range
(0.12-0.21), which is more encouraging than "no signal exists" — it
suggests a ratio-based heuristic could work in principle — but the ratio
that matters (B's cardinality ÷ A's cardinality) is exactly the
cardinality signal §4 already noted Prometheus's engine does not compute
before running a query. Nothing in this benchmark demonstrates a way to
know that ratio in advance from selector shape alone (matcher count,
metric name, etc.): a `{job="a"}` vs `{job="a",env="prod"}` pair could
realize any ratio from near-0 to near-1 depending on the data, and this
benchmark's ratios were set by construction, not inferred from the
matchers. That gap is the same one §4's "static heuristic vs. skip the
decision entirely" choice already identified, now with a number attached
(~0.2) for what the heuristic would need to threshold on, if a proxy for
it existed.

A secondary finding worth flagging for §3's design, independent of the
ratio question: `shared_prototype` allocates roughly 13-14× the memory of
`unshared` at the same cardinality (e.g. 45.5MB vs 3.1MB at
`|A|=10,000, ratio=0.5`), because this prototype decodes every sample into
a `[]fSample` per series rather than reusing buffers. A real
`FilteredSelectorNode` would need CSE's `materialize.go` discipline (reuse
iterators/slices, document non-retention for callers) to avoid trading
query latency for GC pressure even in the ratio range where it wins on
time; this benchmark does not attempt that optimization; it measures the
naive case as a upper bound on the memory cost, not a lower one.

## Recommendation

Do not build `FilteredSelectorNode` or wire subsumption-based sharing into
`promql/engine.go` yet, and the benchmark in §6 sharpens rather than
resolves the concern §4 raised: `shared_prototype` regresses `unshared` at
ratio 0.1 and 0.01 in every cardinality tested, and only wins at ratio 0.5.
Unlike CSE (a strict, unconditional win: identical work computed once
instead of twice) and unlike `MultiAggregation` (marginal, but never
negative), SSE is confirmed here, empirically, to be a pass that regresses
query performance for a large fraction of the shapes it would apply to
(anything where B is meaningfully more selective than A), and there is
still no cardinality-ratio signal in Prometheus's engine to distinguish
those shapes from the ~0.2-and-above ratio range where it helps. Building
`FilteredSelectorNode` today would mean shipping a pass with no reliable
way to gate it, which does not match this project's "prove it doesn't
regress common cases" bar. §5's overlap with `PropagateMatchers` (already
landed) further narrows whatever slice of cases would remain worth
capturing even if a ratio signal existed.

## Open questions

1. Is v1 scope (exact-matcher-superset containment, no regex reasoning)
   worth building at all, or does §5's overlap with `PropagateMatchers`
   already close most of the gap it would fill? Unresolved by §6's
   benchmark, which measures the sharing mechanism's cost profile, not how
   often qualifying selector pairs actually occur in real queries once
   `PropagateMatchers` has already run.
2. §6's benchmark finds the win is ratio-dependent with a crossover around
   0.12-0.21 (stable across a 100× cardinality range), but does not find a
   way to predict that ratio from selector shape alone (matcher count,
   metric name, matcher values) ahead of running the query. Answering this
   fully requires either a cardinality-estimation capability Prometheus's
   engine does not have today, or evidence that some cheap proxy (e.g.
   same-`Name` selectors reliably falling above or below the crossover)
   correlates with the ratio well enough to gate on.
3. Should this extend to `MatrixSelectorNode`/range selectors, or is
   vector-selector-only a reasonable v1 boundary (mirroring how CSE itself
   started narrower, e.g. `hasUnstableOffset`'s conservative
   never-share rule)?
4. Does subsumption need the same `hasUnstableOffset` conservatism CSE
   applies (never share across a subquery/step-invariant boundary), and if
   so, is that guard identical to CSE's existing one or does it need its
   own?

## §7: built anyway, opt-in, always-share

The recommendation above stands as the analysis: this pass has no reliable
gate today. The project chose to build it anyway, behind
`EngineOpts.EnableSubsetSelectorElimination` (default `false`, mirroring
`EnableCommonSubexpressionElimination`'s own opt-in precedent), accepting
the regression risk documented in §6's results as the cost of enabling it.
This section records what shipped and resolves the four open questions
above with the choices actually made, so a future reader does not have to
reconstruct them from the diff.

**What shipped**, in `promql/plan/sse.go`, `promql/plan/materialize.go`, and
`promql/engine.go`:

- `SubsetSelectorElimination` detects the relation exactly as §1 defines
  it — same `Name`/`Offset`/`Timestamp`/`SkipHistogramBuckets`, one
  selector's `LabelMatchers` a strict superset of another's — and, per
  selector, picks the eligible candidate with the *fewest* matchers as its
  source (§2's "cheapest already-materialized superset"), ties broken
  deterministically. It runs after `CommonSubexpressionElimination` on the
  same plan, as §2 specifies.
- `MaterializeSubsetSharing` differs from CSE's own `MaterializeSharing` in
  one deliberate way: it never aliases pointers in the real
  `parser.Expr` tree. B's occurrence keeps its own object; the returned
  `map[parser.Expr]parser.Expr` (subsumed selector → source selector) is
  consumed directly by the evaluator instead. Pointer-aliasing, CSE's own
  mechanism, only works when both occurrences want an *identical* result
  (§3's own point) — it cannot express "derive a filtered subset of."
- Execution reuses CSE's existing `sharedNodeRefcount`/`sharedResults`/
  `cloneSharedValue` machinery rather than introducing a parallel cache: a
  selector serving as another's subset source gets its refcount bumped by
  one per dependent (in addition to its own natural occurrence, or
  whatever real CSE duplication already gave it). `evalUncached`'s
  `*parser.VectorSelector` case, when a subset-source mapping exists,
  calls `ev.eval` on the source (memoized, so a source with more than one
  dependent is still computed once) and filters the resulting `Matrix` by
  the dependent's own `LabelMatchers` — checking the full list, not just
  the residual matchers the source lacks, since re-checking an
  already-guaranteed-true matcher is redundant but harmless and avoids
  computing a residual set at all. No `FilteredSelectorNode` was added to
  `promql/plan`: the relation is tracked as a field
  (`VectorSelectorNode.subsetSource`) on the existing node type instead,
  since the execution side never needed a new plan-graph shape, only a
  new side-table the evaluator consults.
- `populateSeries` is untouched: a subsumed selector's own
  `storage.Querier.Select` call still happens (exactly as CSE also leaves
  a merged selector's *first* `Select` call in place today). This is safe
  to leave as-is because `Select` returns a lazy `storage.SeriesSet`: the
  actual index walk and chunk decode only happen once something iterates
  it, and a subsumed selector's `UnexpandedSeriesSet`/`Series` fields are
  simply never consulted once `evalUncached` redirects it to its source —
  so the real cost §6's benchmark measured is still avoided, without
  needing to touch `populateSeries` at all.

**Open Question 1** (does this overlap with `PropagateMatchers` close most
of the gap): left open. Nothing in this implementation measures real-world
selector-pair frequency; that remains future work if this pass's actual
production impact needs justifying later.

**Open Question 2** (predicting the ratio from selector shape): left open,
as expected — no cardinality estimate was added. This is the
"always-share" choice from this section's title: the implementation does
not attempt to gate on `Name` equality or any other proxy, so it will
regress queries below the ~0.12-0.21 crossover ratio §6 measured, exactly
as documented. This is a known, accepted cost of enabling the flag, not an
oversight.

**Open Question 3** (`MatrixSelectorNode`/range selectors): resolved
narrow, as CSE itself started. A `VectorSelectorNode` reachable only as a
`MatrixSelectorNode`'s child is excluded from candidacy entirely (neither
source nor dependent): `promql/engine.go`'s only execution hook is
`evalUncached`'s `*parser.VectorSelector` case, which a
`MatrixSelectorNode`'s child is never evaluated through (`matrixSelector`
reads `e.VectorSelector.Series` directly instead). A relation spanning a
matrix-nested selector would simply never fire, while still inflating a
source's expected-consumer count — excluding it is simpler than tracking
per-occurrence eligibility for this case. Extending to range selectors, if
ever wanted, is future work, not part of this change.

**Open Question 4** (same `hasUnstableOffset` conservatism as CSE):
resolved yes — identical guard, not a new one. `subsumes` refuses to
relate two selectors if either has `hasUnstableOffset` set, for the exact
reason CSE's own `EquivalentTo` does (see `hasUnstableOffset`'s doc in
`promql/plan/nodes.go`): the same `setOffsetForAtModifier` hazard applies
regardless of which relation (equality or subsumption) would otherwise
expose a query to it.

**One gap inherited from CSE, not introduced by this work**: neither
`plan.FromExpr` nor `CommonSubexpressionElimination`'s `EquivalentTo`
capture `parser.VectorSelector`'s `Smoothed`/`Anchored` fields today, so
CSE can already (in principle) merge two selectors differing only in one
of those flags. `SubsetSelectorElimination` inherits the same gap rather
than fixing it, since fixing it is a pre-existing CSE concern out of scope
for this pass to take on alone — SSE is no less safe than CSE already is
here, not more.

## §8: real-engine benchmark, run

`promql/engine_sse_bench_test.go` (`BenchmarkSubsetSelectorElimination`)
is the real-engine counterpart to §6's prototype, requested by
`docs/query-planner-phase5-productionization.md` item 5: instead of
operating directly on a `storage.Querier`, it builds a real `*promql.Engine`
with `EnableSubsetSelectorElimination` true/false and runs an actual
instant query, `sum(A) + sum(B)`, where A = `{__name__="sse_bench_test",
job="x"}` and B = A plus one residual matcher — the same subsumption
relation, cardinalities (100/1,000/10,000), and ratios (0.50/0.10/0.01) as
§6, reused across ratios via the same
`half`/`decile`/`percentile`-labeled fixture.

Measured on this machine (Apple M4 Pro, `-benchtime=1s -count=6`,
`benchstat` comparing `sse_disabled` against `sse_enabled`; absolute
numbers will vary by hardware):

| \|A\| | ratio | time (sse\_disabled → sse\_enabled) | time delta | alloc delta |
|---|---|---|---|---|
| 100    | 0.50 | 261.2µs → 185.5µs | −28.95% (p=0.002) | +7.95% B/op |
| 100    | 0.10 | 196.8µs → 184.2µs |  −6.44% (p=0.002) | +33.99% B/op |
| 100    | 0.01 | 180.5µs → 184.6µs |  +2.30% (p=0.002) | +42.12% B/op |
| 1,000  | 0.50 | 2.533ms → 1.730ms | −31.71% (p=0.002) | +3.82% B/op |
| 1,000  | 0.10 | 1.871ms → 1.724ms |  −7.86% (p=0.002) | +34.95% B/op |
| 1,000  | 0.01 | 1.715ms → 1.730ms |   ~0.9% (p=0.065, not significant) | +45.63% B/op |
| 10,000 | 0.50 | 26.07ms → 17.70ms | −32.10% (p=0.002) | +2.67% B/op |
| 10,000 | 0.10 | 19.18ms → 17.33ms |  −9.65% (p=0.002) | +32.88% B/op |
| 10,000 | 0.01 | 17.29ms → 17.72ms |  +2.49% (p=0.004) | +42.67% B/op |

This does **not** reproduce §6's ~0.12-0.21 crossover ratio on the time
axis, and the reason is a real difference between the prototype's
comparison and what the real engine actually does, not a case of the
prototype simply being wrong:

1. **§6's prototype measured `Select(A)` and `Select(B)` in isolation —
   nothing else ever needed A's own decoded result.** This benchmark's
   query, `sum(A) + sum(B)`, is the shape SSE actually requires: the
   subsumption relation is only detected between two selectors that are
   *both already present as literal nodes in the query* (§1), so A's own
   value is always consumed by the query regardless of whether SSE is
   enabled. A's selection and decode cost is therefore sunk in both the
   `sse_disabled` and `sse_enabled` case; the only cost SSE actually
   removes is B's *independent* selection and decode. That cost was never
   free to begin with, so removing it is closer to a pure win than §6's
   isolated comparison suggested — down to the lowest ratio tested
   (0.01), the time cost is flat to a barely-significant few-percent
   regression, never the 8-18% one would extrapolate from §6's numbers at
   this ratio.
2. **The allocation numbers tell a different, more consistent story, and
   match §6's underlying mechanism much more closely.** `sse_enabled`'s
   `B/op` overhead grows monotonically as ratio shrinks (roughly 3-8% at
   ratio 0.5, up to 33-46% at ratio 0.01, at every cardinality tested).
   This is exactly §6's finding 1 (`shared_prototype`'s cost is nearly
   flat in absolute terms regardless of ratio, because it is dominated by
   fully materializing A) showing up as a *relative* memory cost instead
   of a *relative* time cost: deriving a tiny B by filtering all of A
   allocates roughly the same amount of memory to do it no matter how
   small B turns out to be, so the smaller B is, the more wasteful that
   fixed cost looks by comparison. Time is largely unaffected because
   A's decode was already unavoidable, but the GC pressure is real and
   scales the same way §6 predicted.

**Conclusion for the risk documented in this file and repeated in
`docs/feature_flags.md`/`cmd/prometheus/main.go`'s `--enable-feature`
wiring**: the ~0.12-0.21 crossover *ratio* does not manifest as a query
*latency* regression in the one query shape SSE can actually fire for
(source and dependent both consumed by the same query), because that
shape always pays the source's decode cost regardless of the flag — this
narrows the practical risk from §6/§7's framing. It does still manifest as
a real, ratio-dependent *memory allocation* cost, which is a real (if
different) concern for GC-sensitive deployments running high-cardinality
low-ratio queries repeatedly. The feature flag's warning should be read as
"expect more allocation at low subset ratios, not necessarily slower
queries in the shape this pass targets" rather than a blanket "may be
slower" — a more precise, less alarming framing than §6/§7 could
support before this benchmark existed, but not a clean bill of health
either.
