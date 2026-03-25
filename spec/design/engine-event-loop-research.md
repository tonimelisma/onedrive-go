# Engine Event Loop: Deep Research & Concrete Solutions

Based on the initial review of `spec/design/engine-event-loop.md`, I've conducted a deep dive into the `internal/sync` package to validate the identified gaps and risks. This document details the exact nature of these issues based on the current codebase and proposes concrete, deeply-researched solutions to fix the design document before execution.

---

## 1. The Bootstrap Deadlock (Critical Pattern Flaw)

**The Issue:** The design plan proposes replacing the two goroutine architecture (main + drain) with a single event loop. However, it fails to account for the blocking behavior of `waitForQuiescence()` during bootstrap.

**Current Code Reality:**
In `internal/sync/engine.go`, `bootstrapSync()` does the following sequentially:
1. Spawns `go func() { e.drainWorkerResults(...) }()`
2. Calls `e.processBatch(...)` which pushes actions to the worker pool.
3. Calls `e.waitForQuiescence(ctx)` which **blocks** until the `DepGraph` is empty.

**Why the Plan Fails:**
If we compress `drainWorkerResults` into the main event loop, we cannot call `waitForQuiescence` sequentially before starting the loop. If the main goroutine is blocked waiting for workers to finish (quiescence), *nothing is processing the worker results*, meaning the actions will never complete, and `waitForQuiescence` will deadlock forever.

**The Solution:**
Bootstrap must be absorbed into the event loop state machine rather than existing as a blocking prefix phase. 

*Revised Design for Bootstrap:*
1. Change `LoopConfig` to accept an initial `[]PathChanges` batch (the bootstrap changes).
2. Start the event loop immediately.
3. Upon entering the event loop, if `bootstrapChanges` exists, process them via `processBatch` over to the outbox/workers.
4. Introduce a `bootstrapPending bool` local state variable in the event loop. While true, observers are NOT started.
5. In the select loop, when `depGraph.InFlightCount() == 0` (or `emptyCh` fires) AND `bootstrapPending` is true:
   - Run `postSyncHousekeeping()`
   - Start the observers (`startObservers`)
   - Bind observer channels (`errs`, `skippedCh`) to the select loop multiplexer.
   - Set `bootstrapPending = false`.

*Why this works:* It allows the single event loop to process bootstrap results, naturally waiting for quiescence without a blocking call, and elegantly transitions into the steady-state watch phase.

---

## 2. RunOnce Lifecycle Inversion (Execution Model Gap)

**The Issue:** Phase 4 says "Refactor `RunOnce`'s `executePlan` to use `runEventLoop` with a one-shot `LoopConfig`." But `RunOnce` relies on a specific shutdown sequence that the event loop doesn't natively support.

**Current Code Reality:**
`executePlan` (for one-shot syncs) does this:
```go
pool.Start(ctx, e.transferWorkers)
go func() { e.drainWorkerResults(...) }()
pool.Wait() // Blocks until all graph items complete
pool.Stop() // Closes the results channel
<-drainDone // Waits for drain to finish side-effects
```

**Why the Plan Fails:**
If `executePlan` calls `runEventLoop()`, it blocks. Inside `runEventLoop`, the loop terminates when the `results` channel closes. But who calls `pool.Wait()` and `pool.Stop()` to close the channel? Neither the caller (blocked on the loop) nor the loop itself (blocked on select) can do it.

**The Solution:**
The `WorkerPool` lifecycle management must be passed into the event loop or managed by an explicit concurrent coordinator for one-shot mode.

*Revised Design for RunOnce:*
In `executePlan`, launch a dedicated lifecycle goroutine:
```go
pool.Start(ctx, e.transferWorkers)

// Lifecycle coordinator
go func() {
    pool.Wait() // Wait for actions to finish
    pool.Stop() // Stop workers AND close results channel
}()

// Run the event loop synchronously in the main thread
err := e.runEventLoop(ctx, &LoopConfig{
    results: pool.Results(),
    bl: bl,
    // other channels nil
})
```
*Why this works:* The separate goroutine waits for the graph to empty, signals shutdown by closing `results`, which cleanly breaks the `runEventLoop` out of its blocking select, maintaining the single-receiver principle.

---

## 3. watchState Field Migration Strategy (Data Structure Gap)

**The Issue:** The plan vaguely dictates "Remove `watchState`... Fields that survive move to `Engine`... or become locals." `watchState` contains 17 critical fields. Flattening them into `Engine` destroys the compile-time distinction between one-shot mode (where these are nil) and watch mode.

**Current Code Reality:**
Fields include: `scopeGate`, `scopeState`, `buf`, `deleteCounter`, `trialPending`, `trialTimer`, `trialMu`, `retryTimer`, `retryTimerCh`, `remoteObs`, `localObs`, `nextActionID`, `lastPermRecheck`, `lastSummaryTotal`, `reconcileRunning`.

**The Solution:**
We must formally map these to preserve boundaries.

1. **Event Loop Locals (inside `runEventLoop`)**:
   - `trialPending` (map converted to straight local state)
   - `trialTimer` (recreated as `time.Timer` locally)
   - `retryTimer` (recreated as `time.Timer` locally)
   - `lastSummaryTotal` (local integer)
   - `activeObs` (local integer for tracking exit conditions)

2. **LoopConfig Fields (Passed in via config)**:
   - `buf`
   - `scopeGate`
   - `scopeState`
   - `deleteCounter`
   - `remoteObs` / `localObs` (Needed to process `Close()`)

3. **Removed Entirely**:
   - `trialMu` (Concurrency eliminated)
   - `retryTimerCh` (Direct `time.Timer.C` access)

*Why this works:* It keeps state strictly scoped. Pushing `scopeGate`, `buf`, etc. into `Engine` directly would pollute one-shot mode with watch-specific objects. `LoopConfig` becomes the new, immutable `watchState`.

---

## 4. `logFailureSummary` and End-of-Batch Semantics

**The Issue:** Currently, `logFailureSummary` runs at the extreme end of `executePlan` (once per sync). The design omits exactly when to aggregate and log these summaries in continuous watch mode.

**Current Code Reality:**
Failure logging relies on an internal `e.syncErrors` map being populated and then flushed as aggregate summaries (e.g., "12 files failed with X"). If never flushed in watch mode, memory leaks and logs never appear.

**The Solution:**
1. In one-shot mode: `logFailureSummary` runs immediately after `runEventLoop` returns.
2. In watch mode: `logFailureSummary` must execute **when the outbox is drained and `depGraph.InFlightCount() == 0`**. This signifies the end of a cohesive "batch" of activity.

---

## 5. Select Starvation & Unbounded Outbox

**The Issue:** The plan predicts 5-6 channels triggering at once (timer, reconcile, read batch, external signals, results). The Go runtime guarantees pseudorandom fairness in `select`, meaning `readyCh` pushes (from the outbox) might be starved while we consume new events, bloating memory.

**Current Code Reality:**
In a major reconciliation event, 10,000+ files might yield results simultaneously. 

**The Solution:**
Do not wait for profiling; implement the inner drain loop aggressively.
```go
case r, ok := <-c.results:
    if !ok { return nil }
    dispatched := e.processWorkerResult(ctx, &r, c.bl)
    outbox = append(outbox, dispatched...)
    
    // INNER DRAIN: prevents outbox bloat during bursts
    for len(c.results) > 0 { // non-blocking check
        select {
        case r2, ok2 := <-c.results:
            if !ok2 { return nil }
            outbox = append(outbox, e.processWorkerResult(ctx, &r2, c.bl)...)
        default: break // Should not hit if len > 0, but safe
        }
    }
```

---

## 6. TODO.md Interaction & Sequencing

**The Issue:** `engine-event-loop.md` describes replacing admission logic utilizing `entry.scopeKey.BlocksAction(...)`. Simultaneously, `TODO.md` dictates moving `BlocksAction` to the `syncdispatch` package as a free function.

**The Solution:**
Strictly sequence the work:
1. **First**, implement `TODO.md` items 1, 4, 5, and 6. This stabilizes the `synctypes` boundaries and gets `BlocksAction` into its final form `syncdispatch.BlocksAction(...)`.
2. **Second**, execute the `engine-event-loop.md` refactor taking advantage of the cleaned-up dependencies, preventing cross-branch merge conflicts.

---

## Refined Migration Roadmap (Recommended Adjustments)

To safely execute the event loop migration, Phase 3 and 4 of the original plan must be adjusted to incorporate these findings:

*   **Phase 3a (Event Loop Structuring)**: Introduce the local `bootstrapPending` state variable into the single event loop. Move the blocking `waitForQuiescence` logic directly into the select case that handles `emptyCh`.
*   **Phase 3b (Config Migration)**: Formalize `LoopConfig` as the new carrier for watch-specific dependencies, ensuring `Engine` doesn't become a god object.
*   **Phase 4 (RunOnce Refactor)**: Introduce the concurrent lifecycle coordinator to call `pool.Wait()` and `pool.Stop()`.

These changes shift the plan from "theoretically sound but practically flawed" to a bulletproof implementation roadmap.
