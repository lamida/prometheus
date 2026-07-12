# Phase 5: productionizing CSE and SSE

Status: not started. This is a punch list for a future session, written
after auditing the state of the `query-planner` branch once both CSE
(`EngineOpts.EnableCommonSubexpressionElimination`) and SSE
(`EngineOpts.EnableSubsetSelectorElimination`) existed. Nothing in this
document has been implemented yet.

## Context

Both optimizations are functionally complete and tested as a *library*
feature: `promql/plan/*.go` implements detection and materialization,
`promql/engine.go` wires execution behind their respective `EngineOpts`
booleans (both default `false`), and both have unit tests
(`promql/plan/*_test.go`) plus engine-level correctness tests
(`promql/engine_cse_test.go`, `promql/engine_sse_test.go`) — differential
suites against a baseline engine, and a full `promqltest` builtin-corpus
run with the feature enabled.

None of that is reachable from an actual `prometheus` binary today. This
document is the list of what's missing to change that, plus two
CSE/SSE-parity test gaps found during the same audit.

## 1. Wire `--enable-feature` flags in `cmd/prometheus/main.go`

`cmd/prometheus/main.go`'s `--enable-feature=` switch (around line 220-330)
has a `case` for every other experimental PromQL knob (`promql-per-step-stats`
at line 251, `promql-delayed-name-removal` at 309,
`promql-extended-range-selectors` at 312, etc.), each setting a field on the
`config` struct that later flows into the real `promql.EngineOpts{}` literal
built around line 988-1004. CSE and SSE have neither a `case` nor a config
field, so there is currently no way to enable either outside a Go test that
constructs `EngineOpts` directly.

Needed:

- Add two `config` struct fields (naming should follow the existing
  pattern, e.g. `promqlEnableCommonSubexpressionElimination` /
  `promqlEnableSubsetSelectorElimination`, mirroring
  `promqlEnableDelayedNameRemoval`).
- Add two `case` arms to the `--enable-feature` switch:
  `promql-common-subexpression-elimination` and
  `promql-subset-selector-elimination` (or shorter names if a maintainer
  review calls for it — check for naming precedent/feedback before
  committing to these).
- **SSE should probably not ship this flag without also documenting, in its
  log message and/or the flag's own description, that it can regress
  queries below roughly a 0.12-0.21 subset-cardinality ratio** (see
  `docs/query-planner-phase4-design.md` §6-§7) — unlike every other
  `--enable-feature` flag in this list, which are unconditional
  improvements or new capabilities, this one has a known, accepted
  performance-regression risk. Precedent: `extra-scrape-metrics`'s case
  already uses `logger.Warn` instead of `logger.Info` for a
  behavior-changing flag; SSE's case should probably do the same, with a
  message that names the risk instead of just "enabled".
- Wire both new config fields into the `promql.EngineOpts{}` literal.
- Update the flag's help text (`a.Flag("enable-feature", "Comma separated
  feature names to enable. Valid options: ...")` around line 645) to list
  both new option names alphabetically.

## 2. User-facing documentation

`docs/feature_flags.md` documents every other `--enable-feature` option
(`promql-per-step-stats`, `promql-delayed-name-removal`,
`promql-extended-range-selectors`, `promql-binop-fill-modifiers`, etc.) but
has no entry for CSE or SSE. Needed: one subsection each, following that
file's existing format (short description, what it changes, any caveats).
SSE's entry in particular should carry the same regression-risk caveat as
its log message above — a user enabling it needs to know it is not a
strict improvement the way CSE is.

## 3. CHANGELOG.md entry

Neither feature has one. Add entries once the CLI wiring lands (an
`EngineOpts`-only, flagless feature arguably doesn't need a CHANGELOG
entry; a real `--enable-feature` option does).

## 4. SSE pool-release test (parity gap with CSE)

CSE has `promql/engine_cse_pool_test.go`, with
`TestCSESharedNode_InstantQueryStillReleasesPool` and
`TestCSESharedNode_NestedSharingResultsStayCorrect`, using a
`drainFPointPool` helper to verify — via real execution, not just correct
query results — that a shared node's pooled `FPoint`/`HPoint` slices
actually get returned to the pool once every consumer is done with them.

SSE's `shouldReleaseSharedNode` redirection through `ev.subsetSource` (see
`promql/engine.go`, the `if src, ok := ev.subsetSource[expr]; ok { return
ev.shouldReleaseSharedNode(src) }` branch) has no equivalent test. All
existing SSE tests (`promql/engine_sse_test.go`) check correctness only.
Needed: a `promql/engine_sse_pool_test.go` (or additions to the CSE pool
test file) that:

- Constructs a query where a source selector has one or more SSE-derived
  dependents, drains the pool before and after, and asserts the source's
  point slices are returned exactly once every dependent has consumed them
  — not leaked (never released) and not double-released.
- Covers the "combined with CSE" case too: a selector that is both a real
  CSE-duplicate (refcount from real AST occurrences) and an SSE source
  (refcount bumped further by dependents) should still release correctly
  once, not once per contributing mechanism.

## 5. SSE real-engine benchmark (parity gap with CSE)

CSE has `promql/multiaggregation_bench_test.go`, which constructs a real
`promql.EngineOpts{EnableCommonSubexpressionElimination: true}` and
`promql.NewEngine`, then benchmarks actual query execution through it.

SSE's only benchmark, `promql/subsetselector_bench_test.go`, predates the
engine wiring: per its own header comment, it operates directly on a
`storage.Querier` as a "hand-rolled prototype" because "there is no
plan-integrated `FilteredSelectorNode` to benchmark" — that was true when
it was written (docs/query-planner-phase4-design.md §6), but is no longer
true now that SSE is wired into `evalUncached`.

Needed: a new benchmark (or an addition to the existing file) that:

- Builds a real engine with `EnableSubsetSelectorElimination: true` vs.
  `false`, over the same fixture data used by `promql/engine_sse_test.go`'s
  `sseTestLoadData` (or a purpose-built, larger fixture — the existing one
  is sized for correctness tests, not performance measurement).
- Runs the same query shapes across the same cardinality/ratio axes the
  standalone prototype benchmark used (100/1,000/10,000 series;
  0.5/0.1/0.01 subset ratios), to check whether the real, engine-integrated
  implementation reproduces the ~0.12-0.21 crossover ratio the prototype
  predicted, or whether the actual overhead profile (map lookups,
  `cloneSharedValue`, refcount bookkeeping) shifts that number.
- Whatever the result, feed it back into `docs/query-planner-phase4-design.md`
  as a §8 the way §6/§7 did for the prototype and the build decision,
  so the doc's crossover-ratio claim is backed by the real implementation,
  not just the storage-level stand-in.

## Suggested order

1 and 2 (CLI + docs) are the highest-value items: right now neither
feature can be used by anyone who isn't writing Go test code against this
fork directly. 4 and 5 (SSE test/benchmark parity) are lower urgency but
should land before recommending SSE for real use, since they are exactly
the kind of gap that "confirmed empirically, not just assumed" — this
project's own stated bias (see `docs/query-planner-phase3-design.md` and
`-phase4-design.md`) — would flag as incomplete.
