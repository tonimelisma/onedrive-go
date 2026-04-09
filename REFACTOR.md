# Second-Stage Architecture Cleanup

## Summary

This file is the implementation brief for the next architecture increment after `#336`.
It is intentionally verbose. The point is not to be short. The point is to make it hard for another agent to misread the current architecture, accidentally revive rejected ideas, or casually mix unrelated local work into the next increment.

The most important facts to start from are these:

- The repo is **not** currently sitting on a clean `main`-like working tree.
- The local checkout has been on a dirty branch named `refactor/second-stage-architecture-cleanup`.
- At the time this brief was researched, the branch tip already matched `origin/main` at `0b6b873`, so there was **no branch-base delta to rebase away**.
- The working tree also contained substantial uncommitted edits, including unrelated changes outside sync architecture work.
- There was no pre-existing `REFACTOR.md`.
- There is an existing historical [`post-split-cleanup-note.md`](/Users/tonimelisma/Development/onedrive-go/spec/archive/design/post-split-cleanup-note.md), so this file uses descriptive section names instead of numeric headings that would conflict with that note.

This document does **not** argue for rolling back the architecture that landed in `#334` through `#336`.
The core architectural direction was right.
The work left is seam cleanup, stress hardening, runtime ownership cleanup, and document reconciliation.

The topics covered here are the ones identified after `#336`:

- result-flow seam
- lifecycle stress coverage
- retry/trial reconstruction contract
- event-loop / package-split docs
- runtime-state extraction

Recommended execution order if these are pursued:

1. Decide runtime-state extraction first.
2. Then either narrow or finish the remaining result-flow cleanup on top of that ownership model.
3. Formalize and harden the retry/trial reconstruction contract.
4. Add lifecycle stress coverage after the code shape stabilizes.
5. Rewrite or archive historical event-loop/package-split docs last, after the architectural end-state is actually chosen.

## Scope And Ground Truth

### What is already correct

The following architectural moves are already the right baseline and should not be rolled back lightly:

- explicit durable store semantics
- single-owner engine loop semantics
- engine-owned retry/trial dispatch instead of synthetic observer events
- removal of the unsupported `400 "ObjectHandle is Invalid"` classifier
- `ObserveSinglePath()` as the per-path local reconstruction entrypoint for retry/trial upload work
- narrower engine file decomposition

These were not cosmetic changes. They removed real ambiguity and split ownership from the runtime.

### What is still open

The remaining work is not “rewrite the engine again from scratch.”
The remaining work is:

- reduce the last meaningful result-flow drift surface
- add real concurrency/lifecycle stress protection
- formalize the retry/trial reconstruction contract so it cannot silently drift
- stop the old proposal docs from acting like live design documents
- decide whether the existing local runtime-state extraction WIP should be finished or rejected

### Local WIP Warning

This must be treated as a first-class implementation constraint.

At the time this brief was researched, there was substantial local uncommitted work that was **not** integrated into `main`.
That is not a minor detail. It changes how follow-up work should be done.

Implications:

- do **not** casually continue future implementation on top of a dirty branch without isolating the intended changes first
- do **not** assume all local edits are part of the sync architecture plan
- do **not** treat local doc edits as accepted architecture just because they exist in the working tree

The next implementing agent should isolate the work first:

- safest option: create a clean worktree from `origin/main`
- alternative: split or stash unrelated changes before continuing
- worst option: continue directly on the dirty branch and accidentally bundle unrelated local work

## Important Internal Interfaces

No public CLI, config, or external API change is required by the recommended options in this brief.

The main internal types and modules implicated by the next increment are:

- [`internal/sync/engine_runtime_types.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_runtime_types.go)
- [`internal/sync/engine_result_flow.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_result_flow.go)
- [`internal/sync/engine_retry_trial.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_retry_trial.go)
- [`internal/syncobserve/single_path.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncobserve/single_path.go)
- [`cmd/devtool`](/Users/tonimelisma/Development/onedrive-go/cmd/devtool)
- [`.github/workflows/ci.yml`](/Users/tonimelisma/Development/onedrive-go/.github/workflows/ci.yml)

The authoritative current design docs that should stay authoritative are:

- [`spec/design/sync-engine.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-engine.md)
- [`spec/design/sync-execution.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-execution.md)
- [`spec/design/sync-observation.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-observation.md)
- [`spec/design/retry.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/retry.md)

The historical proposal/review docs that need explicit treatment later are:

- [`spec/design/engine-event-loop.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/engine-event-loop.md)
- [`spec/design/sync-package-split.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-package-split.md)
- associated `engine-event-loop*` and `sync-package-split*` review docs

## Result-Flow Seam

### Current state

The result-routing architecture is substantially better than it was before `#336`.

What is already true:

- `processResult()` exists as a shared routing skeleton
- `processWorkerResult()` and `processTrialResult()` are thin entry wrappers
- the local WIP on the dirty branch already pushes this further by threading `*engineFlow` and `*watchRuntime` through result handling rather than reaching into mutable runtime state through `Engine`

What is still true:

- normal results and trial results still diverge at the side-effect layer
- `applyResultDecision()` applies only to the normal result path
- `processTrialDecision()` still owns its own success/failure side effects

That trial-specific side-effect surface currently includes:

- `releaseScope`
- `extendScopeTrial`
- retry timer arming
- success counting and scope success recording
- failure error recording

### Why this is still a smell

This is not the old “completely duplicated result systems” problem anymore.
The shared routing skeleton is a real architectural improvement.

The remaining smell is narrower but still real:

- future cross-cutting result side effects can drift between normal and trial handling
- ordering-sensitive code is still split across more than one function
- tests prove important trial semantics, but they do not make the maintenance surface smaller

This means the current code is workable, but it is not yet the smallest or safest shape it could be.

### Architectural invariants that must remain true

Any further cleanup in this area must preserve these rules:

- classification remains independent of trial-ness
- trial-ness is dispatch context, not raw result classification
- trial success must release scope before ready dependents are admitted
- trial failure must extend the trial interval and must not re-enter ordinary scope detection
- shutdown remains a no-record completion path
- permission flow remains part of the normal result path only

These invariants matter because one of the old architectural mistakes was chasing “everything should look identical” too aggressively.
That kind of representational uniformity produced worse code, not better architecture.

### Options

#### Option A: keep the current split and strengthen contract tests only

Keep the current structure:

- shared `processResult()` skeleton
- normal path side effects through `applyResultDecision()`
- trial path side effects through `processTrialDecision()`

Then add stronger regression tests and stop there.

Pros:

- lowest churn
- lowest immediate risk
- easy to land safely
- compatible with either accepting or rejecting runtime-state extraction

Cons:

- leaves the drift surface intact
- future result-policy changes still require two separate mental passes
- stabilizes the current seam instead of simplifying it

When this is reasonable:

- if runtime-state extraction is rejected
- if the team decides the current structure is “good enough” and wants to stop moving code around

#### Option B: unify the routing skeleton further while keeping explicit trial hooks

This is the recommended option if runtime-state extraction is accepted.

Shape:

- keep `classifyResult()` pure
- keep `resultContext{isTrial, trialScopeKey}`
- make `processResult()` the only place that:
  - completes dependency graph nodes
  - decides subtree completion vs dependent admission
  - performs shared post-completion routing steps
- express trial-specific behavior as explicit hook calls rather than a mostly-independent decision function

This is **not** “pretend trial results are normal results.”
It is “keep one routing skeleton and explicit trial semantics.”

Pros:

- reduces drift without lying about trial semantics
- lowers the risk of ordering bugs in future result-policy changes
- composes naturally with `engineFlow` / `watchRuntime`

Cons:

- moderate refactor churn
- needs careful tests to avoid subtle release/extend ordering regressions

When to choose:

- if the team wants the architecture to actually get simpler rather than merely cleaner-looking

#### Option C: collapse everything into one giant `processResult` switch

This should be rejected.

Pros:

- one function
- superficially the most unified representation

Cons:

- branch-heavy
- harder to scan
- easier to regress semantically
- collapses honest distinctions into one control blob

### Recommendation

Choose **Option B** if runtime-state extraction is accepted.
Choose **Option A** only if runtime-state extraction is rejected or deliberately narrowed.

### Tests required

This area should not be considered complete without tests that explicitly lock in:

- trial success releases scope before ready dependents are admitted
- trial failure extends interval and does not feed ordinary scope detection
- normal success still clears item failure only, not full scope state
- shutdown still completes the subtree without dispatch
- permission-driven skip still routes through permission-decision flow only
- dependency-graph completion ordering is identical for trial and non-trial results

## Lifecycle Stress Coverage

### Current state

The repo has strong ordinary unit coverage, but very little true stress coverage.

What exists:

- CI runs `go test -race -coverprofile=... ./...`
- there are already many targeted engine tests for:
  - release/discard
  - trial success/failure
  - retry sweep behavior
  - watch steady-state
  - quiescence
- [`spec/design/sync-execution.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-execution.md) still explicitly marks targeted `-race` stress tests as `[planned]`

What does **not** exist:

- no repeated `-count` runs in CI
- no dedicated stress script
- no focused lifecycle stress profile in [`cmd/devtool`](/Users/tonimelisma/Development/onedrive-go/cmd/devtool)
- no scheduled or manual non-blocking sync stress job
- no required PR stress subset

### Why this matters

The engine is now more state-machine-driven than before.
That is good architecture, but it shifts the remaining regression risk.

The highest remaining risk is not missing ordinary branch coverage.
It is:

- timing-sensitive ordering regressions
- subtle event-loop starvation or sequencing regressions
- retry/trial timing interleavings
- watch bootstrap/quiescence edge cases
- dependency-graph empty/not-empty transition issues

These are exactly the kinds of failures that can sit undetected behind a green ordinary unit suite.

### Options

#### Option A: local-only stress recipe

Document a command or script such as:

```sh
go test -race -count=50 ./internal/sync/...
```

but do not run it in CI.

Pros:

- no CI cost
- simple to adopt

Cons:

- easy to ignore
- not a real guardrail
- weak long-term protection

#### Option B: scheduled or manual non-blocking stress job

Add a dedicated stress job that runs only:

- on schedule
- on workflow dispatch
- or as non-blocking informational CI

Pros:

- catches flakes without blocking normal PRs
- good evidence-gathering first step
- low friction

Cons:

- slower feedback
- easier for failures to be deprioritized

#### Option C: required PR stress subset

Add a curated repeated `-count` stress job for a small set of tests on every PR.

Pros:

- strongest correctness guardrail
- best direct protection against sequencing regressions

Cons:

- increases PR latency
- can create a flake-management tax before the suite is proven stable

#### Option D: staged hybrid

This is the recommended option.

Stage 1:

- create a focused sync stress target
- run it as scheduled/manual or non-blocking CI

Stage 2:

- once stable, promote a small subset to required PR coverage

Pros:

- best long-term outcome with the least immediate risk
- matches `AGENTS.md`: use the durable solution, but earn extra process cost with evidence

Cons:

- two-step rollout

### Recommendation

Choose **Option D**.

### Exact candidate stress surface

This should be concrete, not hand-wavy.
Recommended buckets:

- `releaseScope` / `discardScope` lifecycle invariants
- `runRetrierSweep` with:
  - in-flight suppression
  - batch limits
- `runTrialDispatch` with:
  - success
  - failure
  - stale or missing candidates
  - skipped actionable candidates
- watch bootstrap / `runWatchUntilQuiescent`
- steady-state watch continuation after the graph drains
- dependency graph empty/not-empty transitions
- retry/trial timer interleavings, if there is a deterministic helper path that can exercise them

### Recommended CI shape

Because there was already unrelated local CI/verify WIP around `cmd/devtool verify` profiles, future stress coverage should **not** be casually coupled to that work unless that coupling is deliberate.

The clean future shape is:

- add a new verify profile such as `stress-sync`
- keep it out of the default public verification profile initially
- run it scheduled/manual first
- optionally promote a small subset later

### Tests required

- repeated `-race -count` run for curated `internal/sync` tests
- repeated `-race -count` run for concurrency-sensitive `syncdispatch` / `syncobserve` tests where applicable
- failure output that clearly identifies which bucket flaked

## Retry/Trial Reconstruction Contract

### Current state

This part of the architecture is materially better than it was before `#336`.

What is now true:

- upload-side retry/trial reconstruction uses `ObserveSinglePath()`
- `ObserveSinglePath()` covers:
  - `ShouldObserve`
  - missing-path resolution
  - oversized-file rejection
  - baseline-hash reuse
  - hash-failure empty-hash semantics
  - folder handling
- `runRetrierSweep()` and `runTrialDispatch()` convert actionable single-path skips into durable actionable failures instead of silently dropping them

The current implementation is already backed by targeted regression tests in:

- [`internal/syncobserve/single_path_test.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncobserve/single_path_test.go)
- [`internal/sync/engine_single_owner_test.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_single_owner_test.go)

### What still smells

The design is correct, but it is still a delicate seam.

Important limits and risks:

- it is intentionally **not** a full scan
- it does **not** perform directory-wide collision analysis
- it depends on staying aligned with observer semantics over time
- `isFailureResolved()` still performs pre-check logic separately from `ObserveSinglePath()`
- future scanner/observer rule changes can drift if `ObserveSinglePath()` is not kept in sync

None of these are reasons to revert the design.
They are reasons to formalize the contract and keep the seam visible.

### Explicit contract

`ObserveSinglePath()` should be treated as the canonical engine-owned **per-path** local reconstruction API.

It is intentionally responsible for:

- current-path normalization
- `ShouldObserve`
- actionable local validation failures
- missing-path resolution
- oversize detection
- baseline-hash reuse
- best-effort hashing with empty-hash fallback

It is intentionally **not** responsible for:

- directory-wide collision detection
- recursive scans
- watch debounce semantics
- reconstructing the full observer pipeline

This distinction matters.
One of the easiest mistakes here would be to gradually rebuild the scanner in a second place under the banner of “better parity.”

### Options

#### Option A: freeze the current contract and only document it

Treat `ObserveSinglePath()` as finished and only add docs/tests.

Pros:

- lowest churn
- stable contract surface

Cons:

- still relies on discipline rather than explicit design ownership
- future drift remains possible

#### Option B: formalize `ObserveSinglePath()` as the canonical per-path observation contract

This is the recommended option.

Shape:

- explicitly document that it is the **only** engine-owned per-path local reconstruction API
- treat its current intentional limits as part of the contract
- when local validation changes, update:
  - scanner path logic where relevant
  - `ObserveSinglePath()`
  - shared parity tests

Possible refinement later:

- factor additional shared per-path helper logic into `syncobserve` if duplication grows
- do **not** turn it into a micro-scanner prematurely

Pros:

- preserves semantic honesty
- reduces future drift
- keeps the correct engine-owned retry/trial model

Cons:

- requires discipline
- requires docs and tests to stay in lockstep

#### Option C: revert to synthetic observer/buffer reinjection

Reject this.

Pros:

- superficially appears to maximize parity

Cons:

- semantically dishonest
- reintroduces fake event uniformity
- recreates the old architectural smell the refactor removed

#### Option D: build a fuller single-path micro-scan API

Do not do this now.

Pros:

- stronger parity on paper

Cons:

- risks rebuilding too much of the scanner in another form
- complexity rises quickly
- violates the repo’s simplicity rules unless evidence later justifies it

### Recommendation

Choose **Option B**.

### Worries that must stay explicit

- if scanner semantics change and `ObserveSinglePath()` does not, retry/trial behavior will drift
- the double-stat / pre-check seam is acceptable today, but should stay visible in tests
- hash-failure empty-hash behavior is important and must not be “cleaned up” away
- actionable skip conversion is part of the engine/store contract, not incidental behavior

### Tests required

- missing path resolves cleanly
- baseline-hash reuse works
- hash failure returns an event with empty hash
- invalid name becomes an actionable skip
- path too long becomes an actionable skip
- oversized file becomes an actionable skip
- internal exclusions resolve without noise
- skipped held trial candidate becomes actionable and trial selection continues
- only skipped held candidates release scope cleanly

## Event-Loop / Package-Split Docs

### Current state

The docs are in an in-between state.

Good:

- [`spec/design/engine-event-loop.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/engine-event-loop.md) and [`spec/design/sync-package-split.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-package-split.md) already have historical notes near the top
- [`spec/design/sync-engine.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-engine.md) is the current authoritative design doc
- [`cmd/devtool`](/Users/tonimelisma/Development/onedrive-go/cmd/devtool) already checks a few stale phrases

Still bad:

- historical docs still contain active-looking implementation prescriptions
- `engine-event-loop.md` still includes claims like:
  - delete `watchState`
  - grep should find zero `watchState`
  - adopt specific timer/event-loop cleanup mechanics
- `sync-package-split.md` still mixes contradictory transition language such as “will extract watchState” and later “done”
- review docs still live under `spec/design/`, which visually places them close to authoritative live design docs
- current stale-phrase checks are too narrow to guarantee truthfulness

### Why this matters

The problem is no longer “are those docs useful at all?”
They are useful as history and rationale.

The problem is that they are still easy to misread as design targets rather than postmortem material.
That creates a real maintenance risk: another agent can re-follow rejected ideas because the repo still presents them in design-doc clothing.

### What implementation taught us that the old docs got wrong

This section matters more than the file moves.
The repo should not preserve the old docs in a way that hides what implementation actually taught us.

The old docs were wrong or over-prescriptive about several things:

- forcing retry/trial work through synthetic event or buffer semantics
- deleting `watchState` just because it existed
- overvaluing representational uniformity over semantic honesty
- keeping unsupported quirks as standing runtime behavior
- prescribing more flattening and package motion than the final implementation actually needed

The major lesson is this:

- the big architectural diagnosis was often right
- the proposed mechanical cure was not always right

That distinction needs to be preserved explicitly.

### Options

#### Option A: leave docs where they are and rely on the historical-note banner

Pros:

- minimal churn
- preserves history

Cons:

- stale mechanics remain searchable inside `spec/design/`
- future agents will keep revisiting rejected ideas

#### Option B: move historical docs to an archive/history location unchanged

Pros:

- clearly separates current design from history
- lowers confusion immediately

Cons:

- stale content remains mostly unchanged
- path churn without enough content improvement

#### Option C: rewrite the historical docs into short ADR-style records, then archive or clearly classify them

This is the recommended option.

Shape:

- authoritative current design docs stay in `spec/design/`
- historical proposal and review docs become concise records of:
  - what was proposed
  - what was accepted
  - what was rejected
  - why
- remove pseudo-current implementation instructions and grep-based completion criteria from historical docs

Pros:

- preserves rationale
- stops stale mechanics from masquerading as current architecture
- captures actual hindsight from implementation

Cons:

- real documentation work
- requires explicitly stating that some earlier ideas were wrong

#### Option D: delete the historical docs after extracting useful conclusions

Pros:

- cleanest design tree

Cons:

- loses rationale and review context
- makes future architectural choices harder to explain

### Recommendation

Choose **Option C**.
If the repo later wants stronger hygiene, combine **Option C + Option B**.

### What must be stated explicitly

This file and the future doc cleanup should state:

Current authoritative docs:

- [`spec/design/sync-engine.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-engine.md)
- [`spec/design/sync-execution.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-execution.md)
- [`spec/design/sync-observation.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/sync-observation.md)
- [`spec/design/retry.md`](/Users/tonimelisma/Development/onedrive-go/spec/design/retry.md)

Historical docs:

- `engine-event-loop*.md`
- `sync-package-split*.md`
- associated review documents

Cross-cutting worry:

- the dirty branch already contained edits to `sync-engine.md` describing runtime-state extraction
- those edits should **not** land unless the runtime-state extraction decision is actually accepted

## Runtime-State Extraction

### Ground truth first

At the time this was researched:

- `HEAD == origin/main == 0b6b873`

So there was **no actual rebase work left** in the sense of branch-base divergence.
The real question here is not “rebase first.”
The real question is:

- what should be done with the existing local runtime-state extraction WIP that is already sitting on top of that base?

### What already existed in the dirty working tree

The local WIP was already substantial:

- a new [`internal/sync/engine_runtime_types.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_runtime_types.go)
- `engineFlow`
- `oneShotRunner`
- `watchRuntime`
- propagation through:
  - [`internal/sync/engine_loop.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_loop.go)
  - [`internal/sync/engine_result_flow.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_result_flow.go)
  - [`internal/sync/engine_retry_trial.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_retry_trial.go)
  - [`internal/sync/engine_watch.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_watch.go)
  - [`internal/sync/engine_scope_lifecycle.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_scope_lifecycle.go)
  - tests and docs
- `go test ./internal/sync/...` passed on that WIP
- `go test ./internal/syncobserve/...` passed on that WIP

That means this is not a half-broken sketch.
It is a serious in-progress refactor that already proved basic mechanical viability.

### What problem runtime-state extraction is solving

Even after `#336`, `Engine` on `main` still conceptually mixes:

- immutable dependencies and collaborators
- one-shot mutable execution state
- watch-only mutable runtime state
- counters
- error slices
- shortcut snapshots

The local WIP attacks that directly:

- `Engine` becomes an immutable dependency container plus public entrypoints
- `engineFlow` owns common per-run mutable execution state
- `watchRuntime` owns watch-mode mutable state
- `oneShotRunner` owns one-shot mutable state

### Why this matters architecturally

This is not just aesthetic cleanup.
It changes the ownership model from:

- one broad orchestrator struct that still carries too much runtime baggage

to:

- one immutable dependency container
- one common run-scoped state object
- one watch-mode runtime owner
- one one-shot runtime owner

That is a cleaner end-state if it can be landed without bundling unrelated work.

### Options

#### Option A: do nothing and keep `Engine` broad

Pros:

- lowest immediate risk
- avoids finishing a wide refactor on top of a dirty local branch

Cons:

- leaves the main residual ownership smell in place
- weakens the architectural story we want the docs to tell
- strands viable local WIP

When this is reasonable:

- only if the team decides the benefit is not worth the mechanical churn

#### Option B: narrow version, extract watch runtime only

Shape:

- keep `watchRuntime`
- leave one-shot mutable state mostly on `Engine`
- avoid introducing `engineFlow` / `oneShotRunner` fully

Pros:

- smaller change set
- addresses the watch-only ownership problem directly

Cons:

- makes one-shot/watch symmetry worse
- leaves shared result/counter/shortcut state conceptually split
- likely less coherent than the already-existing local WIP

#### Option C: finish the current full extraction

This is the recommended option.

Shape:

- `Engine` is an immutable dependency container plus public entrypoints
- `engineFlow` owns common run-scoped mutable execution state
- `watchRuntime` embeds or composes `engineFlow` and adds watch-only state
- `oneShotRunner` embeds or composes `engineFlow` and owns one-shot execution

Pros:

- strongest ownership model
- aligns with the best current architectural story
- local WIP already proved this shape is feasible
- sync and syncobserve tests already passed in the WIP

Cons:

- broad mechanical propagation
- high merge/conflict risk if mixed with unrelated dirty changes
- doc updates must not get ahead of the accepted code decision

#### Option D: go further into a larger coordinator/package rewrite

Reject this for now.

Examples of what not to do now:

- introduce a new coordinator package
- build interface-heavy loop abstractions
- do a larger package split around runtime state

Pros:

- might look cleaner in a diagram

Cons:

- too much simultaneous churn
- not justified before the simpler extraction is finished
- conflicts with the repo’s simplicity guidance

### Recommendation

Choose **Option C**, but only in an isolated clean implementation context.

That means:

- do **not** finish it by casually continuing on the dirty branch as-is
- isolate the runtime-state extraction work from unrelated local changes first
- then either transplant the existing WIP cleanly or reapply it deliberately

### What work is actually left if Option C is chosen

The local WIP is not “done” just because targeted package tests passed.
The remaining work is mostly integration, cleanup, and explicit architectural decisions.

#### Isolate the work from unrelated dirty branch edits

This is mandatory.

The local dirty branch already included unrelated edits across areas such as:

- `graph`
- `config`
- `trustedpath`
- CI / verify
- other untracked internal packages

Do not silently drag those along with the runtime refactor.

#### Finish propagation consistently

If `engineFlow` / `watchRuntime` / `oneShotRunner` land, they need to be the real mutable runtime owners.
That means:

- remove or rewrite stale comments that still refer to `e.watch`, `watchState`, or older ownership assumptions
- avoid leaving fallback helper paths that still reach into `Engine` as if it owned mutable runtime state directly

#### Decide the timer-ownership story explicitly

The local WIP still used `time.AfterFunc` signaling channels in [`internal/sync/engine_results.go`](/Users/tonimelisma/Development/onedrive-go/internal/sync/engine_results.go).

That is probably acceptable if:

- callbacks only signal the loop
- callbacks do not mutate loop-owned state directly

But this needs an explicit architectural judgment.
The repo should say whether this is:

- the intended end-state

or

- a deferred cleanup item

Do not leave it ambiguous.

#### Finish test harness cleanup

The local test harness already knew about `watchRuntime` / `engineFlow`.
That is good.

What remains is:

- strengthen those helpers
- remove bypass paths that let tests ignore the new ownership model
- make runtime ownership the default test surface, not an optional code path

#### Update docs only after the code choice is locked

The local dirty branch already contained doc edits that assumed runtime-state extraction was accepted.
Those doc edits should land only if the code decision lands.

### Specific worries that must stay visible

- the current branch is dirty with many unrelated edits
- implementation must isolate the runtime refactor work first
- local doc edits should not be treated as accepted architecture automatically
- timer signaling still uses callback-based wakeups and should be decided explicitly
- if runtime-state extraction lands, the result-flow seam should be re-evaluated on top of the new ownership model rather than the old `Engine`-owned shape

## Cross-Cutting Worries And Smells

These are not tied to one section, but another agent should see them immediately.

- the historical [`post-split-cleanup-note.md`](/Users/tonimelisma/Development/onedrive-go/spec/archive/design/post-split-cleanup-note.md) already exists and uses different numbering, so `REFACTOR.md` must keep named sections
- the dirty branch included unrelated edits outside sync architecture work; do not bundle them accidentally
- `sync-engine.md` was already being edited in ways that assume runtime-state extraction is accepted
- verify/CI WIP is separate work unless deliberately coupled
- historical docs are still close enough to live docs that another agent could easily re-follow rejected mechanics
- landing exactly on the current coverage threshold is operationally brittle; future refactors should add headroom, not merely preserve the minimum

## Test Plan

The eventual implementation of the recommended path should require:

- `go test ./internal/sync/...`
- `go test ./internal/syncobserve/...`
- full `go test -race -coverprofile=/tmp/cover.out ./...`
- coverage comfortably above the floor, not exactly on it

If lifecycle stress coverage is adopted:

- a focused repeated `-race -count` stress target for sync lifecycle tests

If the doc rewrite work lands:

- doc-consistency checks for stale historical phrases after the rewrite

If runtime-state extraction lands:

- dedicated tests proving `engineFlow` / `watchRuntime` / `oneShotRunner` are the mutable runtime owners

## Assumptions And Defaults

- This brief assumes the numbered items refer to the post-`#336` architecture cleanup list, not the historical [`post-split-cleanup-note.md`](/Users/tonimelisma/Development/onedrive-go/spec/archive/design/post-split-cleanup-note.md).
- There was no rebase delta at research time because the dirty branch base already matched `origin/main`.
- No public API change is required for the recommended options in this brief.
- The recommended overall path is:
  1. finish the current full runtime-state extraction in an isolated branch
  2. further unify result-routing side effects on top of that ownership model
  3. formalize `ObserveSinglePath()` as the canonical per-path retry/trial local reconstruction contract
  4. add staged lifecycle stress coverage
  5. rewrite or archive historical event-loop/package-split docs only after the architectural end-state is actually accepted

## Bottom Line

The architecture does **not** need another ground-up rethink.

The remaining work is:

- clarify ownership further
- reduce the last meaningful result-flow seam
- formalize the retry/trial reconstruction contract
- add real lifecycle stress protection
- stop historical proposal docs from acting like live design documents

The largest immediate decision is runtime-state extraction.

The most important repo fact discovered during research is:

- there already **is** a serious local implementation of that refactor
- it is not in `main`
- it is mixed with unrelated local edits
- it appears viable
- it should be isolated and either finished or consciously rejected, not ignored
