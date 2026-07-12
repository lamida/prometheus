# Phase 2 design: a DAG-capable plan graph for CSE, MultiAggregation, and EliminateDeduplicateAndMerge

Status: design-discussion draft, not an accepted design. This is the artifact
for the pre-implementation discussion Phase 2 of `docs/query-planner-analysis.md`
calls for before any code lands — it exists to be argued with, not merged as-is.

## Context

`docs/query-planner-analysis.md` identified seven of Mimir's PromQL optimizer
passes as portable to Prometheus's single-node engine, with no dependency on
tenancy, sharding, or remote execution. Four of them — `ReduceMatchers`,
`PropagateMatchers`, `RemoveStaticallyEmptyExpressions`, and one still
pending — are pure `parser.Expr` → `parser.Expr` rewrites and need nothing
beyond the `promql/optimize` framework already merged to this branch (#4, #6).

The remaining three — `CommonSubexpressionElimination` (CSE),
`MultiAggregation`, and `EliminateDeduplicateAndMerge` — are a different
order of problem. In Mimir, CSE works by physically converting the plan tree
into a DAG: it detects a duplicated subexpression and replaces every
occurrence with a pointer to one shared node, then execution wraps that
shared node in a streaming buffer so each of its several parents can read
its result once without recomputing it. `MultiAggregation` depends on that
same sharing to find multiple aggregations consuming one shared node and
fuse them. Prometheus's evaluator has no DAG, no plan layer at all, and —
more specifically — no per-series streaming execution model: `rangeEval`
(`promql/engine.go:1410-1587`) materializes a full `Matrix` per node for the
entire query range in one call, and recycles point slices in place via
`putFPointSlice`/`putHPointSlice` (`engine.go:1572-1578`) immediately after
the current (single) consumer reads. A shared node has more than one
consumer, so a naive port risks a second consumer reading memory the first
consumer's pooling has already recycled — exactly the hazard Mimir's
streaming buffer exists to prevent, and Prometheus has no equivalent buffer
to reach for.

This document proposes a concrete design anyway, because "CSE is hard" is
not, by itself, a reason to leave it unspecified — it's a reason to design
the hard part deliberately, in the open, before any evaluator-internals code
is written. Everything below is a proposal to be challenged, not a plan
already committed to.

## Goals / non-goals

**Goals:**
- A plan representation that lets `CommonSubexpressionElimination` and
  `EliminateDeduplicateAndMerge` be implemented and reviewed independently of
  the harder execution-strategy question.
- An execution strategy for shared plan nodes that gets CSE's primary
  benefit (not recomputing a duplicated subexpression) without requiring
  Prometheus's evaluator to become a second, parallel execution model.
- Explicit, testable correctness guarantees for sample-limit accounting and
  for structural equality in the presence of subqueries/offsets/`@`.

**Non-goals for this design:**
- Matching Mimir's memory-saving profile for CSE. Mimir's streaming buffer
  bounds peak memory for a shared subexpression to a thin sliding window
  between its fastest and slowest consumer; the strategy proposed below does
  not, and that tradeoff is made explicitly (see §4).
- A general-purpose streaming rewrite of Prometheus's evaluator. That would
  get Mimir's actual memory profile but requires two execution models to
  coexist with a handoff at the boundary — out of scope here, and flagged in
  §4 as a possible future phase if this design's memory profile proves to be
  a real problem in practice.

## 1. The `Node` interface

New package `promql/plan/`, distinct from the already-merged `promql/optimize/`
(which operates on `parser.Expr` trees and never needs this package).

```go
package plan

// Node is a node in the query plan graph. Unlike a parser.Expr tree, a Node
// may have more than one parent: CommonSubexpressionElimination introduces
// sharing by pointing multiple parents at the same Node.
type Node interface {
	ChildCount() int
	Child(idx int) Node
	SetChildren(children []Node)
	ReplaceChild(oldChild, newChild Node)

	// ParentCount reports how many other Nodes currently reference this one
	// as a child. Nodes built by FromExpr start at 1; SetChildren and
	// ReplaceChild adjust it on the child side as edges are added or removed.
	ParentCount() int

	// EquivalentTo reports whether two nodes are structurally interchangeable
	// for CSE purposes: same operation, same fully-resolved parameters
	// (matchers, range, offset, timestamp — see §3), ignoring child identity.
	EquivalentTo(other Node) bool

	Describe() string // For debugging/plan-explain output.
}
```

This diverges from Mimir's `Node` (`planning/plan.go:123` in
`github.com/grafana/mimir`) in two ways, both deliberate:

- **`ParentCount()` is explicit, tracked bookkeeping**, not something derived
  by walking the graph. Mimir's execution model is per-series pull-based
  streaming: each consumer of a shared node just tracks its own read cursor,
  and nothing needs to know in advance how many consumers exist. Prometheus's
  proposed execution strategy (§4) needs to know exactly when the *last*
  consumer has read a shared node's result, so it can release pooled memory
  — re-deriving that by re-walking the whole plan from the root on every
  node visit would be needlessly expensive.
- **No execution/materialization method on `Node` itself.** Mimir's plan
  node is tied 1:1 to a streaming operator. Here, `Node` stays purely
  structural — shape and equivalence only — and "how does this node
  execute, especially when shared" lives in a separate layer (§4). This
  means the DAG/CSE-detection logic (§2, §3) can land and be reviewed on its
  own merits, execution-inert, before the harder execution question is
  settled at all.

## 2. AST → plan-graph conversion

`promql/plan/build.go`: `func FromExpr(expr parser.Expr) (*QueryPlan, error)`,
a straightforward recursive conversion — one plan node type per concrete
`parser.Expr` type (`VectorSelectorNode`, `MatrixSelectorNode`,
`BinaryExprNode`, `AggregateExprNode`, `CallNode`, `SubqueryExprNode`, and so
on). No sharing is introduced at this stage; CSE discovers and introduces it
afterward as a separate pass, mirroring Mimir's own `nodeFromExpr`
(`planning.go:462`).

This conversion runs **after** `PreprocessExpr`, not before, unlike the
Phase 1 `promql/optimize` passes (which run before it, per PR #4's design).
Reasoning: by the time `PreprocessExpr` has run, step-invariance has already
been decided and encoded as the synthetic `StepInvariantExpr` wrapper, and
durations/offsets/timestamps have been resolved from `DurationExpr` into
concrete values. The plan graph should see that resolved state directly, as
a plain attribute on the corresponding node, rather than needing to
recompute it or handle a wrapper node — `ParenExpr` and `StepInvariantExpr`
are dropped/unwrapped during conversion for exactly this reason.

## 3. Structural equality and path-dependence

This is the sharpest correctness edge in the whole design, and the one
still flagged as genuinely uncertain (see Open Questions).

`populateSeries` resolves `offset`/`@`-timestamp state **path-dependently**
as it walks the AST: a `VectorSelector` nested inside a `SubqueryExpr` gets
its effective `Offset` adjusted by the subquery's own offset and range (see
`SubqueryExpr.Offset`'s doc comment in `promql/parser/ast.go`: "the offset
used during query execution ... calculated using the original offset ...
and subquery offsets in the AST tree"). Two textually-identical selectors —
same metric name, same `LabelMatchers` — appearing under different subquery
nestings can therefore end up with **different** effective `Offset`/
`Timestamp` once `PreprocessExpr` has run, even though nothing about their
matchers differs.

The proposed rule: `EquivalentTo` must compare each node's **fully-resolved**
fields as they stand *after* `PreprocessExpr` and plan conversion — concrete
`Offset time.Duration`, `Timestamp *int64` values already baked into the
plan node — never the raw, pre-resolution AST fields
(`OriginalOffsetExpr`, path-relative state). Because plan conversion (§2)
happens after `PreprocessExpr`, this resolved state is available directly on
each node by the time CSE runs, so comparing it doesn't require CSE to
re-derive path context itself the way a naive AST-level comparison would
have to.

**This is flagged as uncertain, not settled**, and is exactly the kind of
thing a design discussion should pressure-test before code: it's inferred
from reading Prometheus's own subquery/offset handling, not from a direct
side-by-side comparison against how Mimir's own CSE implementation handles
(or sidesteps) the equivalent question. If Mimir's execution model doesn't
have this path-dependence at all — plausible, since Mimir's subquery/offset
semantics may differ from Prometheus's in ways this analysis hasn't
verified — then this section is solving a problem Mimir's design didn't need
to solve, which is fine, but should be said explicitly rather than implied.

A `SortLabelsAndMatchers`-equivalent normalization pass (sort matchers within
each selector, sort commutative `VectorMatching`/`Grouping` label lists) must
run once, before CSE detection, as its own small plan-level pass — not folded
into `EquivalentTo` itself, since that would re-sort on every pairwise
comparison instead of once up front.

## 4. Execution strategy for shared nodes

Three strategies were considered.

**A. Refcounted deferred pooling — proposed for v1.**
When the plan has a node with `ParentCount() > 1`, compile its evaluation
into a wrapper that: (a) evaluates the shared subtree exactly once into a
`Matrix`; (b) hands that same `Matrix` (or a shallow copy of the outer slice
structs, copy-on-write if a consumer needs to mutate in place) to each
consumer; (c) only calls `putFPointSlice`/`putHPointSlice` once a per-node
counter — initialized to `ParentCount()`, decremented as each consumer
finishes reading — reaches zero.

This is the smallest change relative to today's execution model: no
streaming rewrite, just a refcount check at the pooling call sites.
**Tradeoff, stated plainly**: peak memory for a shared subexpression is held
from its first consumer's read to its last, which — because Prometheus
currently materializes a node's whole range in one call rather than
streaming — can mean "for the rest of the query's evaluation" in the worst
case. This gives up Mimir's memory-saving profile for CSE. What it keeps is
CSE's other benefit: not recomputing the shared subexpression, which is
often the cost users actually notice (e.g., not re-selecting and
re-summing the same `sum(rate(x[5m]))` twice in one query).

**B. Clone-on-share — fallback if A proves too invasive.**
Deep-copy the `Matrix` for every consumer beyond the first, instead of
refcounting. No shared-mutation hazard, no refcount bookkeeping, but
duplicates the *shared result* (not the recomputation) per consumer.
Simpler to implement and reason about than A. Worth falling back to if
auditing every pooling call site (there may be more than the two cited
above — `functions.go` needs its own check) makes A's refcounting too
error-prone to land with confidence.

**C. Scoped streaming rewrite — explicitly out of scope for v1.**
Rewrite evaluation of just the CSE-shared subtrees to a small pull-based
buffer, Mimir-style, leaving the rest of `rangeEval` untouched. Gets Mimir's
real memory profile, but requires two execution models to coexist with a
conversion at the boundary, and doesn't actually shrink peak memory below
what A already achieves unless the consumers *above* the shared node are
also streaming — the classic "no benefit unless the whole downstream
pipeline is streaming too" problem. Worth revisiting only if A's memory
profile turns out to be a measured problem in practice, not speculatively.

**Recommendation: A.** It is the only option that gets CSE's main benefit
without requiring a second execution model, with a change surface that is
small and auditable (a refcount field plus the pooling call sites).

**Known limitation found by post-landing adversarial review, not caught before merge:** the refcount `plan.MaterializeSharing` assigns a node is the number of textual occurrences that existed pre-CSE, not the number of times that node will actually be independently re-evaluated at runtime. Those two coincide for a node at the *top* of a shared subtree, but not for a node *strictly beneath* another shared node in the same subtree (e.g. `rate(foo[5m])` used twice: the `Call`, `MatrixSelector`, and `VectorSelector` nodes are all independently tracked with the same refcount, but only the `Call`'s two occurrences are ever actually visited — once the `Call` is served from cache on its second occurrence, `eval` never redescends to revisit its children). The descendant's release counter then never reaches zero, so its pooled point slices are never returned to the pool for that query. This is a missed-reuse performance regression, not a correctness bug (no double-release, no wrong result — verified by both a differential test and a dedicated adversarial review pass; Go's garbage collector still reclaims the memory once the query's evaluator is no longer referenced), and it does not affect single-level shares (where the tracked node's own release attempts all happen within the same `rangeEval` call, correctly). A correct general fix would compute each node's true expected release count from how many of its ancestors are themselves genuinely re-evaluated rather than from its raw pre-CSE occurrence count — real, nontrivial work, deliberately deferred as a documented follow-up rather than rushed into the trickiest part of this mechanism. See `shouldReleaseSharedNode`'s doc comment in `promql/engine.go` and `TestCSESharedNode_NestedSharingResultsStayCorrect` in `promql/engine_cse_pool_test.go`.

A second, now-fixed bug from the same review: `rangeEval`'s instant-query shortcut returned before reaching the release loop at all, so *no* shared node ever got released for an instant query, single-level or not. Fixed by extracting the release loop into `releaseOrigMatrixes` and calling it from both the shortcut and the general path; regression test `TestCSESharedNode_InstantQueryStillReleasesPool`.

## 5. Sample-limit accounting

`ev.currentSamples` must charge a shared subexpression's samples **exactly
once**. Under strategy A, the increment must live in the "evaluate the
shared node for the first time" branch only, never in the "hand out the
already-computed result to a later consumer" branch. This needs a concrete
regression test, written before or alongside the refcounting implementation
(not after): a query with a CSE-eligible shared subexpression, run with
`maxSamples` set exactly at the correct total (shared-subexpression samples
counted once, plus the rest of the query) should succeed; one sample below
that should fail. This is the kind of bug that's silent — wrong results or
wrong limit enforcement, not a crash — until specifically tested for.

## 6. `EliminateDeduplicateAndMerge` and `MultiAggregation`

- **`EliminateDeduplicateAndMerge`**: Mimir deliberately keeps this
  tree-shaped and runs it *before* CSE, specifically to keep the plan a
  simple tree for as long as possible. Proposed: port that ordering as-is —
  implement it as a `promql/plan`-level pass, run before CSE introduces any
  sharing, with no execution-strategy work needed since it never touches a
  shared node.
- **`MultiAggregation`**: depends on CSE having run first, and keys off
  `ParentCount() > 1` (analogous to Mimir's pointer-identity check on its
  `Duplicate` node type) to find multiple aggregations consuming the same
  shared node and fuse them into one multi-output node. This needs its own
  execution-strategy question answered — a multi-output aggregation node
  producing more than one `Matrix` from a single node is a new evaluator
  capability, independent of the shared-subtree pooling problem in §4 — and
  should be its own design pass and PR series once CSE has landed and
  proven the DAG/refcounting machinery out, not bundled with CSE itself.

## Proposed PR sequencing

Each step below is meant to be reviewable in isolation, with the hardest,
most behavior-changing step deliberately last and narrowest:

1. `promql/plan` package: `Node`, `FromExpr` (§1-§2). Zero behavior change —
   the plan graph is built but nothing consults it yet. Reviewable purely on
   IR design.
2. `EliminateDeduplicateAndMerge`-equivalent (§6): tree-shaped, no DAG/pooling
   risk, still execution-inert if the plan isn't wired into execution yet.
3. CSE detection + `EquivalentTo`/normalization (§3): still plan-level only,
   no execution wiring — reviewable purely as "does this correctly detect
   duplicate subtrees," with a test matrix covering subquery/offset/`@`
   combinations given §3's flagged uncertainty.
4. Strategy-A refcounted execution wiring (§4) + the sample-accounting
   regression test (§5): the actual behavior-changing step, reviewed most
   carefully — ideally by whoever owns `engine.go`'s pooling code today.
5. `MultiAggregation`-equivalent (§6), once CSE has landed and soaked.

## Open questions

1. Is comparing fully-resolved post-`PreprocessExpr` `Offset`/`Timestamp`
   actually sufficient for sound CSE equality (§3), or is there a subtler
   path-dependent hazard not caught here — e.g. interaction with
   `SkipHistogramBuckets` or `LookbackDelta`-derived state? Needs a concrete
   subquery+offset+CSE test case worked through by hand before §3 is trusted.
2. Is Prometheus's subquery/offset path-dependence (§3) actually a problem
   Mimir's own CSE implementation also has to solve, or does Mimir's
   execution model sidestep it in a way this analysis hasn't verified?
3. What is the true count and location of pooling call sites analogous to
   `putFPointSlice`/`putHPointSlice` (`engine.go:1572-1578`) that strategy A
   must intercept? The two cited lines are a starting point, not a verified
   complete list — `functions.go`'s function-specific evaluation paths need
   auditing before §4 is implemented.
4. Where should this discussion actually happen before code is written —
   a Prometheus GitHub issue, the prometheus-developers mailing list, or a
   dev summit slot? This affects timeline and who needs to weigh in, and is
   a judgment call for whoever is driving this work, not decided here.
