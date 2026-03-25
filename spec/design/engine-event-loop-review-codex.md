# Engine Event Loop Refactor Review

## Purpose

This document records my independent point of view on the proposed sync-engine event-loop refactor.

It is intentionally based on the governing requirements, the current design docs, and the current code. It does not treat the existing proposal documents as authoritative. They were useful as prompts, but this review does not assume their conclusions are correct.

This write-up is self-contained so it can be read without cross-referencing the chat that produced it.

## Scope Reviewed

I based this review on the following repository material:

- `AGENTS.md`
- `spec/requirements/sync.md`
- `spec/design/sync-engine.md`
- `spec/design/sync-execution.md`
- `spec/design/sync-observation.md`
- `spec/design/sync-planning.md`
- `spec/design/sync-store.md`
- `spec/design/system.md`
- `TODO.md`
- `spec/design/engine-event-loop.md`
- `spec/design/engine-event-loop-review.md`
- `spec/design/engine-event-loop-research.md`
- `spec/design/engine-event-loop-review-antigravity.md`
- `internal/sync/engine.go`
- `internal/sync/engine_shortcuts.go`
- `internal/sync/permission_handler.go`
- `internal/sync/permissions.go`
- `internal/syncdispatch/dep_graph.go`
- `internal/syncdispatch/scope_gate.go`
- `internal/syncdispatch/scope.go`
- `internal/syncexec/worker.go`
- `internal/syncobserve/buffer.go`
- `internal/syncobserve/scanner.go`
- `internal/syncobserve/observer_local.go`
- `internal/syncplan/planner.go`
- `internal/syncstore/store_admin.go`
- `internal/syncstore/store_failures.go`

No conclusions in this document rely on additional research beyond that set.

## Executive Summary

My conclusion is:

- The refactor should move forward.
- The architectural target should be a single owner for watch-mode engine state.
- The current watch architecture has real structural defects that are worth fixing before launch.
- The current proposal bundles too many independent changes together.
- The event-loop merge is the right direction.
- The proposed removal of `reobserve` is not ready.
- The proposed empty-hash retry strategy is not acceptable.
- The implementation plan should be narrowed, staged more aggressively, and driven by characterization tests.

The short version is:

- Refactor the control topology.
- Do not change trial semantics casually while doing it.
- Do not encode retry intent by corrupting the data model.
- Land the low-risk fixes first.
- Use the event loop to simplify ownership, not to smuggle in behavior changes that still need design work.

## Current Code Reality

The current engine already has the correct high-level pipeline:

- observers produce events
- the buffer batches them
- the planner decides actions
- workers execute actions
- the store persists outcomes

That pipeline is not the problem.

The problem is that watch mode splits control and state ownership across two goroutines:

- `runWatchLoop` owns batch intake, recheck ticks, reconciliation starts, and observer failure handling
- `drainWorkerResults` owns worker-result handling, dependent release, retry sweeps, and trial dispatch

As a consequence, watch-mode state is spread across:

- duplicated admission logic
- multiple timers
- shared mutable maps and slices
- mutexes and atomics added for coordination rather than domain meaning

The most important examples are:

- `admitAndDispatch` and `admitReady` duplicate admission behavior
- `processWorkerResult` and `processTrialResult` duplicate large parts of result routing
- `trialPending`, timers, sync error accumulation, shortcut snapshots, and permission state are all shared across goroutine boundaries

This is exactly the kind of architecture that becomes expensive and fragile when you try to extend failure handling, RPC status, or future watch behavior.

## What Is Actually Good in the Current System

Several parts of the current implementation are sound and should be preserved:

- The event-driven pipeline itself is correct.
- The worker pool is correctly kept as a pure executor.
- `DepGraph` correctly centralizes dependency completion outside workers.
- `ScopeGate` is the right abstraction for scope admission.
- The buffer is the correct place for cross-observer batching and debounce.
- Reconciliation is already correctly prevented from calling `processBatch` directly from a side goroutine.

The refactor should preserve those strengths.

The right question is not "should we replace the current model entirely?"

The right question is "how do we preserve the current pipeline while eliminating split ownership inside the engine?"

## Main Recommendation

The engine should be refactored toward a single event loop for watch mode.

That recommendation is grounded in the code, not in the proposal documents.

I would move forward with that refactor because it directly addresses the biggest structural problems:

- duplicated admission
- duplicated result routing
- shared mutable watch state
- semantically unrelated locking
- hard-to-see ordering constraints

However, I would not move forward with the full proposal as currently written.

I would explicitly narrow the plan to:

1. unify admission
2. unify result processing
3. move watch mode to a single event-loop owner
4. keep live trial probing semantics intact during that move
5. defer optional behavior changes until after the topology is stable

## Findings

## 1. The Single-Owner Event Loop Is the Right Architectural Direction

This is the strongest conclusion from the review.

The current code makes the same conceptual decision twice:

- ready actions produced from planning go through one admission path
- ready dependents produced from completion go through another admission path

That duplication exists because two goroutines both need to make stateful admission decisions.

As long as that split remains, future features will keep fighting the topology.

A single event loop would make the following properties structurally true:

- one admission path
- one result-routing path
- one owner of watch-mode counters and state
- timer state local to the owner
- ordering constraints expressed by straight-line code rather than inter-goroutine convention

That is a real architectural win.

## 2. The Current Refactor Plan Bundles Architecture and Behavior Too Aggressively

The proposal currently treats these as part of one plan:

- event-loop ownership
- `reobserve` removal
- lazy upload hashing
- bootstrap rewrite
- `RunOnce` lifecycle rewrite
- reconciliation completion rewrite
- permission caching cleanup
- file split
- state flattening

Those are not one change.

Some are architecture.
Some are behavior.
Some are optimization.
Some are code organization.

Bundling them together increases review risk, test-debugging risk, and rollback cost.

The event loop is justified even if some of the other ideas are rejected.

That is how the implementation should be staged.

## 3. Duplicated Admission Is the Highest-Value Defect to Eliminate First

The current duplication between `admitAndDispatch` and `admitReady` is not cosmetic.

It is a correctness risk because both functions do meaningfully similar but not identical work:

- trial interception
- stale trial cleanup
- scope admission
- failure recording for blocked actions
- dispatch-state transitions

The two paths are close enough to drift but far enough apart that drift is easy to miss.

I agree with the proposal's core instinct here:

- one `admit` function is the right target

I do not think that unification needs to wait for the full event-loop merge.

In fact, I would do it before the topology change so the highest-risk phase has less moving logic.

## 4. `processWorkerResult` and `processTrialResult` Should Be Unified

This is the second most obvious structural duplication.

Both functions:

- classify results
- complete actions in the dependency graph
- route dependents
- perform stateful side effects

The real distinction is not "trial code versus normal code."

The real distinction is:

- what side effects should success trigger?
- what side effects should failure trigger?
- should scope detection run?

That means one `processResult` function with explicit `isTrial` branches is the correct design shape.

Again, this can be done before the event-loop topology merge.

## 5. Removing `reobserve` Is Not Yet Well Founded

I do not agree that the `reobserve` removal is ready.

The proposal is right that `reobserve` adds complexity.

But the current implementation does more than just "probe the same scope again."

It preserves an important semantic distinction:

- trial dispatch currently uses a live source of truth
- retry sweep uses cached state reconstruction

That distinction matters.

Today, `runTrialDispatch` calls `reobserve`, and `reobserve` can distinguish cases like:

- live remote item still exists and scope still blocks
- live remote item is gone
- local upload source is gone

If trial dispatch is changed to use `createEventFromDB` and worker execution alone, the result model changes.

The main unresolved case is remote 404 during a scope trial.

Today that can be observed before dispatching a full action.
Under the proposal it becomes a worker result, and current classification routes 404 as transient requeue.

That means:

- a dead trial candidate can keep extending a scope interval
- a scope whose entire candidate set vanished can become slow to clear
- the resulting behavior is only "eventually self-healing," not obviously correct

That may still be acceptable, but it is not a drop-in simplification. It is a behavior change and should be treated that way.

My recommendation:

- keep `reobserve` through the event-loop migration
- revisit its removal only after the trial-state model is rewritten explicitly

## 6. The Empty-Hash Retry Idea Is Not Acceptable

This is the weakest part of the current proposal.

The proposal suggests skipping upload retry hashing and driving planner behavior by feeding an empty hash into the local event.

That is a design smell for two reasons:

- it turns a domain value into an implicit control flag
- it couples the engine to a current planner implementation detail

The planner currently detects local change by comparing `Local.Hash` against `Baseline.LocalHash`.

Using `""` to force inequality means the retry path is no longer expressing truth about the file. It is expressing desired planner behavior through a fake value.

That is brittle.

If lazy upload retries are desirable, the clean options are:

- add explicit retry intent to the event or path view
- add an engine-only fast path that bypasses ordinary change detection
- share scanner-style stat matching and only hash when truly needed

The current empty-hash idea should be rejected.

## 7. The Right Fast Path Already Exists in the Scanner and Should Be Reused

The scanner already has the correct fast path:

- compare size and mtime to baseline
- apply a racily-clean guard
- skip hashing only when that check is truly safe

That logic lives in `internal/syncobserve/scanner.go`.

Meanwhile `observeLocalFile` in the engine always hashes.

So there is a real issue here, but the right fix is not a hack.

The right fix is:

- extract the mtime+size+racy-clean decision into shared logic
- use it in both scanner and engine retry observation

That delivers a real win:

- less blocking file I/O in retry and trial paths
- shared behavior between local scanning and engine-side local re-observation
- no distortion of the event model

This change is good regardless of the event-loop migration.

## 8. `handle403` Needs the Prefix Fast Path

This finding is strong and low risk.

The current `handle403` path goes from worker 403 straight into:

- shortcut resolution
- remote permission query
- boundary walking
- failure recording

It does not first ask whether the failing path is already under a known denied prefix.

That means a cluster of sibling failures can repeat expensive boundary work unnecessarily.

This is exactly the kind of bug that should be fixed now and independently of the event-loop work.

The right behavior is:

- first check whether the path is already under a denied boundary
- if yes, avoid another API walk
- if no, do the expensive confirmation and cache the result

This is a clean, standalone improvement.

## 9. Bootstrap and `RunOnce` Lifecycle Need Explicit Design, Not Sketches

The current bootstrap and one-shot code are extremely lifecycle-sensitive.

Bootstrap today depends on this ordering:

- initialize watch infrastructure
- start result drain
- dispatch bootstrap work
- wait for quiescence while results are being processed
- only then start observers

One-shot execution depends on this ordering:

- start workers
- drain results concurrently
- wait for graph completion
- stop workers
- let results channel close
- finish side effects

Any event-loop plan that does not explicitly model those lifecycles is incomplete.

The current proposal gestures at these flows, but it does not fully pin down:

- who waits for workers in one-shot mode
- who closes results
- when the event loop returns in one-shot mode
- how bootstrap transitions from "no observers yet" to steady-state watch
- whether bootstrap is a separate loop invocation or a state within the same loop

That is not fatal, but it means the plan is not ready to implement exactly as written.

## 10. Reconciliation Should Stop Writing Engine State Cross-Goroutine

This is a correct design target.

Today the reconciliation goroutine refreshes shortcuts directly after feeding events.

That is exactly the kind of last cross-goroutine write that should disappear when the engine becomes single-owner.

A completion channel for reconciliation is a clean fix.

I agree with the design direction here:

- reconciliation goroutine performs blocking observation and DB work
- it sends a result back
- the event loop updates engine-owned state

That change fits the target architecture well.

## 11. Keep a Grouped Watch/Event-Loop State Object

I do not agree with flattening everything onto `Engine` just because synchronization goes away.

The current `watchState` has problems because it is shared across goroutines with locks and atomics.

That does not mean the answer is "no state grouping."

A grouped state object still provides useful boundaries:

- watch-only versus one-shot-only state
- timer ownership
- observer handles
- in-memory counters
- reconciliation status
- scope/trial state

My recommendation is:

- keep a grouped watch/event-loop state container
- make it single-owner
- remove locks and atomics from it

That preserves architectural clarity without preserving the concurrency bug source.

## 12. Some Proposal Critiques in the Existing Review Docs Are Wrong or Overstated

A few points from the existing review documents should not drive the implementation plan:

- The claim that event-loop trials would newly incur debounce latency is wrong. Trial dispatch already injects into the buffer today.
- The claim that retrier hashing is primarily a catastrophic stall issue is overstated. It is still worth fixing, but the strongest justification is duplicate work and consistency with scanner semantics.
- The idea that the event-loop topology depends on deleting `reobserve` is wrong. The topology win stands on its own.

The implementation plan should be built from verified code behavior, not from which review memo sounded most confident.

## The Weak Spots in the Current Codebase

These are the weak spots I consider most important before implementation work begins.

## A. Split Ownership of Watch Control Flow

The watch engine currently has two centers of control:

- the watch loop
- the drain loop

That split is the root cause of most of the complexity.

## B. Admission Logic Is Duplicated

This is the highest-risk defect because it duplicates core business logic rather than scaffolding.

## C. Result Routing Is Split Between Trial and Non-Trial Paths

The code currently encodes one conceptual decision tree as two separate result-processing functions.

## D. Timer Ownership Is Artificially Complicated

The timer design currently uses:

- timers
- callback goroutines
- signal channels
- mutex coordination

That is the kind of control machinery a single-owner event loop can and should delete.

## E. Permission Denial Handling Is Too Expensive Under Burst Failures

This is a real inefficiency with a clean fix.

## F. Engine-Side Local Re-Observation Ignores the Scanner's Existing Fast Path

This is duplicate logic and duplicated I/O cost.

## G. The Engine Is Still a Large, Multi-Concern Type

The event-loop refactor will improve correctness and ownership.
It will not solve the entire "god object" problem.

That is acceptable, but it should be stated clearly.

## Recommended Revised Plan

This is the plan I would actually implement.

## Phase 0: Characterization and Invariant Tests

Before touching topology, lock in tests for the behaviors that must not move:

- blocked actions are recorded before their dependents are completed
- trial failure does not feed scope detection
- trial success clears scope and unblocks failures
- bootstrap does not start observers until the initial action graph drains
- one-shot execution drains all worker results before reporting
- permission recheck clears scope immediately when access is restored
- reconciliation completion updates shortcut state on the engine side only

This phase reduces fear later.

## Phase 1: Unify Result Processing Under the Existing Topology

Do this before the event-loop merge.

Goals:

- merge `processWorkerResult` and `processTrialResult` into one `processResult`
- keep current behavior
- keep `reobserve`
- do not change goroutine topology yet

This makes later review dramatically easier.

## Phase 2: Unify Admission Under the Existing Topology

Do this next.

Goals:

- one `admit` function
- one stale-trial cleanup path
- one scope-admission path
- one place that decides whether dispatch-state transition happens

Again, keep current behavior and current goroutines for this phase.

## Phase 3: Land the Low-Risk Independent Fixes

These do not need to wait for the event loop:

- add `handle403` denied-prefix fast path
- extract shared stat-match logic from the scanner
- apply that logic to engine local re-observation
- add any missing oversize-file guards to engine local re-observation

These are worth doing even if the topology work paused.

## Phase 4: Move Watch Mode to a Single Event Loop

Only after Phases 0 through 3.

Goals:

- one owner of watch-mode state
- one watch-mode select loop
- results, batch intake, retries, trials, rechecks, and reconciliation completion all handled by that owner
- remove engine-local watch locks and atomics that only existed for cross-goroutine coordination

Important constraint:

- keep `reobserve` for this phase

The point of this phase is topology simplification, not trial-behavior redesign.

## Phase 5: Align `RunOnce`

Once watch mode is stable and proven, align one-shot execution with the same event-loop/result-processing model.

One-shot is simpler than watch mode, but its lifecycle still needs to be explicit.

Do not try to solve it in the same PR as the watch topology swap.

## Phase 6: Revisit Optional Simplifications

Only after the new topology is stable.

Candidates:

- remove `reobserve`
- simplify trial candidate representation
- simplify or remove remaining timer indirection
- consider deeper engine decomposition

This phase is optional and should be design-driven, not assumed.

## Specific Decisions I Recommend

## Move Forward With

- a single-owner watch-mode event loop
- one unified `admit`
- one unified `processResult`
- reconciliation completion via event-loop-owned state update
- removal of engine-local synchronization that only exists for split ownership
- shared scanner-style local stat fast path
- `handle403` prefix caching

## Do Not Move Forward With Yet

- deleting `reobserve`
- encoding upload retry intent as an empty hash
- flattening all watch-only state onto `Engine`
- a big-bang implementation PR that changes topology and behavior together

## Research and Analysis Still Needed Before Implementation

The plan is not ready to implement until the following are written down clearly.

## 1. Trial Outcome Matrix

We need an explicit matrix for:

- trial success
- trial 429
- trial 507
- trial 5xx outage
- trial 404 remote missing
- trial local source deleted
- trial actionable skip
- trial permission-related failure

For each case the design must specify:

- whether the scope stays blocked
- whether the interval extends
- whether the candidate failure is cleared
- whether the scope clears immediately
- whether dependents are cascaded, retried, or discarded

That matrix is currently implicit in code and proposal prose. It needs to be explicit before `reobserve` is touched.

## 2. Channel and Lifecycle Contract

We need a short design document that states for every channel:

- creator
- writer
- reader
- closer
- shutdown behavior

At minimum this should cover:

- `readyCh`
- worker results
- observer errors
- skipped-item forwarding
- buffer output
- reconciliation completion
- bootstrap emptiness signal

This is already called out as a planned need in the design docs, and the event-loop refactor makes it mandatory.

## 3. Bootstrap State Machine

We need a precise answer to:

- is bootstrap a separate event-loop invocation or an internal mode/state?
- when exactly do observers start?
- what event marks "bootstrap done"?
- what happens if context is canceled during bootstrap?

The answer must be executable, not just aspirational.

## 4. One-Shot Lifecycle Contract

We need a precise answer to:

- who waits for graph completion?
- who stops workers?
- what closes results?
- what condition causes the one-shot event loop to return?

The current implementation answers those questions; the new design must do so just as concretely.

## 5. Sequencing With `TODO.md`

`TODO.md` already proposes moving scope logic out of `synctypes`.

That directly affects:

- admission code
- scope classification
- scope-humanization code paths

This does not have to be done first, but the sequencing has to be decided before implementation begins, or the refactor will churn on unstable boundaries.

## 6. Stress-Test Plan

Before implementation, define the targeted stress tests:

- worker result burst while outbox is non-empty
- retry sweep while batches continue arriving
- trial timer firing under active result flow
- reconciliation completion while batches and results are both active
- shutdown during permission walk
- shutdown during reconciliation
- race runs with repeated counts

The event-loop refactor should be proven under those conditions, not just unit-tested at the happy path.

## 7. State Boundary Decision

We need an explicit decision on whether watch-mode runtime state becomes:

- fields on `Engine`
- a dedicated event-loop state struct
- a revised `watchState` with single-owner semantics

My recommendation is a dedicated event-loop or watch runtime state object.

That should be decided up front.

## Final Recommendation

The engine should be refactored toward a single-owner watch-mode event loop before launch.

That is the right long-term direction and the current code justifies it.

But the implementation plan should be revised before coding starts.

My recommended implementation stance is:

- accept the architectural goal
- reject the empty-hash retry approach
- postpone `reobserve` removal
- front-load characterization tests
- land low-risk fixes early
- separate topology changes from behavior changes

If that is how the work is staged, I think the refactor is worth doing now.

If the current all-in-one plan is used as-is, I think the risk is unnecessarily high for no corresponding architectural benefit.
