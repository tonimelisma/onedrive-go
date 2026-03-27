# Engine Event Loop Refactoring: deep-dive Review

> Historical note: This review captures an earlier analysis of the event-loop proposal before the single-owner implementation landed. The current implemented architecture is documented in `spec/design/sync-engine.md` and `spec/design/sync-execution.md`. References here to `ScopeGate`, `drainWorkerResults`, and `onScopeClear` are preserved only for historical context.

This document provides an independent, deep-dive architectural review of the `engine-event-loop.md` refactoring plan. It examines the assumptions, verifies the logic against the current codebase, highlights edge cases, and provides specific recommendations to strengthen the final implementation.

## 1. Executive Summary

The transition from a multi-actor state-sharing model (`runWatchLoop` + `drainWorkerResults` + `watchState` mutexes) to a pure Event Loop model is strategically sound and well reasoned. It directly addresses the root causes of the "D1-D10" defects outlined in the plan. The approach of preparing the codebase (Phase 2) prior to the topology change (Phase 3) is a mature method to de-risk the migration.

However, the plan has a few subtle flaws:
1. **The "Lazy Hash" Hack** introduces corrupted domain semantics just to trigger a planner side-effect.
2. **Phase 3a is a "Big Bang" commit** which limits bisectability.
3. **The "Zombie Scope" Edge Case** (via `reobserve` elimination) will lead to infinite backing-off trials for remotely-deleted folders.

This review unpacks these findings and provides actionable improvements.

---

## 2. Validation of Core Decisions

### 2.1 The Event Loop Unification (Agree)
Moving all shared mutable state (`trialPending`, `watchShortcuts`, `syncErrors`, timers) into a single goroutine is exactly correct. Go's concurrency model favors passing data over channels rather than sharing memory; the existing code violated this by wrapping state in `RWMutex` / `Mutex` / `atomic` and scattering access across `main` and `drain`. The event loop will eliminate non-deterministic ordering bugs.

### 2.2 Synchronous Permission Handling (Agree)
Decision 1 correctly argues against async I/O for `handle403` and `recheckPermissions`. Given the latency math (one ~1s stall per prefix, then cached), an async state machine would be classic over-engineering. It's a pragmatic call to accept bounded blocking in a background daemon.

### 2.3 Elimination of `reobserve` (Agree)
Decision 2 elegantly removes the orchestrator's duplicate GET request. Letting the normal worker execution act as the scope clearance probe via HTTP 429/507 removes 85 lines of blocking API code from the orchestrator and simplifies timer management. *(Note: See section 3.3 for the 404 boundary case).*

---

## 3. Disagreements and Identified Flaws

### 3.1 Decision 3: The "Lazy Hash" Hack for Upload Retries
**The Plan's Proposal:** When `createEventFromDB` creates an event for an upload retry, it skips the expensive file hash and instead populates the `Hash` string with `""` (empty string) to "trick" the planner into generating an upload action (because `"" != baseline.LocalHash`).

**Why this is flawed:**
1. **Domain Model Corruption:** You are deliberately constructing an invalid `ChangeEvent`. A file change event must have a valid hash. Empty strings are currently validly used by *folders*. Overloading `""` on a file to secretly mean "pretend this changed" is a hack that will inevitably break downstream systems (e.g., debugging tools, logging, or future validators).
2. **Implicit Coupling:** The event loop is tightly coupling itself to an implementation detail of the planner (that the planner uses a simple `!=` string comparison on hashes).

**The Native Solution:**
Do not lie in the data model. If an event is a retry, track that explicitly. The `ChangeEvent` struct can be given an `IsRetry bool` or `ForceUpload bool` field. The `planner.Plan()` logic can then be mechanically updated:
```go
// In planner
if event.ForceUpload || local.Hash != baseline.LocalHash {
    // Generate upload action
}
```
This preserves the truthfulness of the structs and avoids creating "magic values" like an empty string hash for files.

### 3.2 Phase 3a is a "Big Bang" Migration
**The Plan's Proposal:** In one commit, unify admission, replace three select loops, transition `processBatch` return types, rip out `reobserve`, and rewrite `RunWatch` to use the new `LoopConfig`.

**Why this is flawed:**
If tests begin failing subtly (e.g. sporadic deadlocks or event starvation), bisecting a commit that simultaneously touched control flow, initialization, and data flow is impossible.

**The Native Solution:**
Break Phase 3a into at least three sequential commits:
1. **Refactor Admission (Data Flow):** Change `processBatch` to return `[]*TrackedAction` and funnel its output through `admit()` without changing the main/drain goroutines yet.
2. **Abstract the Loop (Initialization):** Introduce `LoopConfig` and `runEventLoop`, but only use it for the one-shot pipeline (`RunOnce`). Prove the abstraction works.
3. **The Topology Swap (Control Flow):** Point `RunWatch` to use `runEventLoop` and delete the old main/drain goroutines.

### 3.3 The "Zombie Scope" Edge Case
**The Plan's Proposal:** Eliminating `reobserve` removes the ability to immediately detect a quick `404` clearance of a scope block. The document calls this "suboptimal but self-correcting" because the retrier's `isFailureResolved` will eventually realize the target is gone.

**The Reality:**
If a user triggers a folder-level 429, the engine scopes out that path. If the user then deletes the folder remotely (to fix the block), the local engine will dispatch trials for the blocked items. The worker will attempt the action, immediately get a 404 (because the folder is gone), and `processResult` will treat it as a standard failure, *extending the trial interval*. 
Because `isFailureResolved` relies on intermittent DB delta scans, it could take hours to clear out 10,000 files in a zombie scope via slow background retrier sweeps, all while the engine exponentially backs off on 404 trials.

**The Native Solution:**
Introduce a fast-path scope clearance inside `processResult`. If an action flagged as `IsTrial` returns an HTTP 404, it means the scope block's target origin no longer exists. Do not extend the trial interval. Instead, immediately call `e.onScopeClear(ctx, r.TrialScopeKey)` to destroy the scope block and unqueue everything behind it.

---

## 4. Nuance Corrections & Refinements

### 4.1 De-atomic `reconcileRunning`
The plan proposes leaving `reconcileRunning` as an `atomic.Bool` to act as a CAS guard. But research of the codebase confirms `runFullReconciliationAsync` is ONLY triggered by the `reconcileC` timer inside the watch loop.
If the single event loop owns the timer, then nothing else can concurrently trigger reconciliation.
**Recommendation:** Remove the `atomic.Bool`. Add a plain boolean `e.isReconciling` to the event loop's memory. When `<-c.reconcileTick` fires, if `e.isReconciling` is true, simply `return`. When `<-c.reconcileDoneCh` fires, set `e.isReconciling = false`. 

### 4.2 Context Plunging during Event Loop Stalls
The plan correctly identifies that `handle403` will block for ~1s per prefix. However, if a user sends `SIGINT` (triggering `ctx.Done()`), the `select` is stuck inside the blocking call, delaying graceful shutdown.
**Recommendation:** Ensure `ctx` from the event loop is plunged deeply into the `walkPermissionBoundary` HTTP calls. The `net/http` client will immediately abort the in-flight TCP requests upon context cancellation, instantly waking the event loop for a clean exit.

### 4.3 `os.Stat` Latency Spikes in the Retrier
The retrier sweep runs in the event loop and calls `createEventFromDB` -> `observeLocalFile` -> `os.Stat`.
While `os.Stat` is ~0.1ms on local SSDs, on fragmented disks or network-attached mounts (SMB/NFS), `os.Stat` can block for 10ms–100ms *per file*. A `runRetrierSweep` batch of 1,000 files could stall the entire event loop for 10+ seconds.
**Recommendation:** Audit `retryBatchSize`. Ensure it is tuned extremely low (e.g. 50-100) or governed by a time-limit slice so that slow `os.Stat` calls don't starve the `ready` debounce channel.

---

## 5. Summary of Recommended Enhancements

When advancing with the `engine-event-loop.md` refactor, apply the following adjustments:

1. **Reject the "Empty String" Hash Hack:** Modify `synctypes.ChangeEvent` to include a `ForceUpload bool` flag for retrier sweeps.
2. **Add 404 Scope Fast-Fail:** In `processResult`, if `isTrial == true && r.HTTPStatus == 404`, trigger immediate scope release via `onScopeClear`.
3. **Drop `atomic.Bool`:** Let the event loop manage reconciliation concurrency via a local, plain boolean.
4. **Enforce Context Termination:** Ensure all blocking API calls (`handle403`, graph lookups) respect the loop's context for crisp shutdown.
5. **Phase 3 Split:** Do not execute Phase 3a in a single commit; isolate data flow, initialization, and control flow changes.
