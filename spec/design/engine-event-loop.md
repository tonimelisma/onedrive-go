# Engine Event Loop Refactoring

> Historical note: This document captures the pre-implementation proposal state for the event-loop refactor. The current implemented architecture is documented in `spec/design/sync-engine.md` and `spec/design/sync-execution.md`. Names such as `ScopeGate`, `drainWorkerResults`, and `onScopeClear` are preserved here for historical context and are no longer the current runtime model.

## 1. Why This Refactoring

### Context

The sync engine is feature-complete for launch. Every requirement from R-2.1 through R-2.16 and R-6.1 through R-6.8 is verified or implemented. The engine handles failure scenarios (429, 507, 5xx, permissions, disk space, crash recovery) correctly.

But the engine's internal structure has concrete defects that make it expensive to modify and fragile to extend. Before launch — zero users, zero backwards-compatibility constraints — is the cheapest time to fix structural problems. After launch, every refactoring carries regression risk against live user data. Per CLAUDE.md: "Prefer large, long-term solutions over quick fixes. Do big re-architectures early, not late."

### Defects

Every defect listed here was verified against the codebase as of the refactoring start. Line numbers are approximate — the file split in Phase 1 will move code.

**D1: Duplicated admission logic.** Two functions implement near-identical scope-gate + trial-interception logic with different output semantics:

- `admitAndDispatch` (engine.go:1506, 40 lines): Main goroutine. Sends admitted actions to `readyCh` via channel write.
- `admitReady` (engine.go:1546, 59 lines): Drain goroutine. Returns `[]*TrackedAction` for the outbox slice.

Both check `trialPending` under `trialMu`, both call `scopeGate.Admit`, both cascade-record scope-blocked failures. `admitReady` includes a stale-trial cleanup path (clearing sync_failures for trials whose scope no longer blocks the action type) that `admitAndDispatch` handles through a separate `handleTrialInterception` function with subtly different semantics. A bug fix in one path can be forgotten in the other. This is the highest-severity defect — it's a class of bug, not a single bug.

**Why this exists**: Two goroutines both need to make admission decisions (the main goroutine after planning a batch, the drain goroutine after completing a dependent action). Channel semantics prevent sharing the return path, forcing the duplication.

**D2: Semantically-wrong mutex sharing.** `retryTimer` is guarded by `trialMu` (engine.go:1954). The retry timer and the trial timer are unrelated subsystems — retry scheduling selects from `sync_failures` by `next_retry_at`, while trial dispatch probes scope blocks via `AllDueTrials`. They share `trialMu` because both were added to `watchState` and a single lock was convenient.

This isn't a bug today — contention is low and both paths are short. But it couples systems that should be independent: changing trial locking semantics could break retry timer behavior, and vice versa. The coupling is accidental, not intentional design.

**D3: Implicit ordering invariants across goroutine boundaries.** Five ordering constraints are enforced by code sequencing within a single function body, but would break silently if a refactor split them across an async boundary:

1. `cascadeRecordAndComplete` must record the scope-blocked failure BEFORE `depGraph.Complete` returns dependents. Both happen in the same function call — Go guarantees statement ordering — but splitting recording from completion across goroutines would break the invariant.
2. Trial fallback must clear stale failures BEFORE normal admission (`admitReady` line 1559).
3. `onScopeClear` must run BEFORE admitting dependents (`processTrialResult` line 1910).
4. Scope detection must NOT be called for trial failures (the "A2 bug" — re-detecting would overwrite the doubled interval with a fresh initial interval).
5. `retryBatchSize` must limit per-sweep work to prevent event loop stalls.

All five are documented in comments. None are enforced by the compiler. A refactor that moves any of these across an async boundary (goroutine, channel, callback) would break correctness without any test catching it until a specific failure scenario occurs.

**D4: Cross-goroutine shared mutable state.** The main goroutine (`runWatchLoop`) and drain goroutine (`drainWorkerResults`) share mutable state through 4 mutexes and 4 atomics in the engine:

| Shared state | Protection | Writers | Readers |
|-------------|-----------|---------|---------|
| `trialPending` | `trialMu` Mutex | Drain (`runTrialDispatch`) | Main (`admitAndDispatch`), Drain (`admitReady`) |
| `trialTimer` / `retryTimer` | `trialMu` Mutex | Both (`armTrialTimer`, `armRetryTimer`) | Both (timer channels) |
| `watchShortcuts` | `watchShortcutsMu` RWMutex | Main (`setShortcuts`) | Drain (`getShortcuts` for 403 handling) |
| `syncErrors` | `syncErrorsMu` Mutex | Drain (`recordError`) | Main (`logFailureSummary`, `resultStats`) |
| `permissionCache` | `permissionCache.mu` RWMutex | Drain (`handle403` → `set`), Main (`recheckPermissions`) | Main (`deniedPrefixes`) |
| `succeeded` / `failed` | `atomic.Int32` | Drain (`applyResultSideEffects`) | Main (`resultStats`) |
| `nextActionID` | `atomic.Int64` | Main only (`executePlan`, `dispatchBatchActions`) | — |

Every new feature that touches engine state must reason about which goroutine accesses it and what protection it needs. Adding a new access site without the correct lock is a data race that the compiler cannot catch.

Additionally, `DepGraph` (in `syncdispatch/`) has its own mutex and atomics (`mu`, `total`, `completed`, `depsLeft`, `closeOnce`, `emptyOnce`) because it's called from both goroutines. `ScopeGate` (in `syncdispatch/`) has its own mutex for the same reason.

**D5: 3,633-line file with 88 methods.** `engine.go` has 88 methods on `*Engine` plus 11 standalone functions. Finding a specific method requires searching. Understanding the flow from "file changed" to "upload complete" requires reading across methods that are hundreds of lines apart. `engine_shortcuts.go` adds another 609 lines and 11 methods.

**D6: handle403 redundant boundary walks.** `handle403` (now in `permission_handler.go`) does not check denied prefixes before calling `walkPermissionBoundary`. With 8 workers, 8 simultaneous 403s for paths under the same denied prefix trigger 8 sequential boundary walks (~1s each). No scope block exists for remote 403 (`ScopeKeyForStatus` returns zero for 403), so the scope gate does not prevent further dispatch to denied paths within the same pass. The protection is planner-level (denied prefixes suppresses uploads on the next pass), not admission-level.

**D7: Three separate select loops.** The engine has three distinct select loops that duplicate the outbox pattern, result processing, and shutdown handling:

1. `drainWorkerResults` (engine.go:2751): 5 cases — outbox, results, trialTimer, retryTimer, ctx.Done. Actor-with-outbox pattern.
2. `waitForQuiescence` (engine.go:1210): 3 cases — emptyCh, ticker.C, ctx.Done. Bootstrap quiescence wait.
3. `runWatchLoop` (engine.go:1232): 7 cases — batch, skipped, recheckTick, reconcileTick, observerErrs, ctx.Done. Main watch loop.

The drain loop and watch loop together contain the full engine behavior but split it across two goroutines with different output semantics. The quiescence loop exists only for bootstrap. nil channels disable cases per mode (e.g., `retryTimerChan()` returns nil in one-shot), but this pattern is applied inconsistently.

**D8: processWorkerResult / processTrialResult duplication.** `processTrialResult` (engine.go:1906, 43 lines) duplicates the dependent routing logic from `processWorkerResult` (engine.go:1818, 35 lines). Both call `classifyResult` and `depGraph.Complete`. Both route dependents (success → admit, shutdown → completeSubtree, failure → cascadeFailAndComplete). The only differences are 3 side-effect branches gated on whether the result is a trial:

1. Success: `onScopeClear` (trial, releases entire scope) vs `clearFailureOnSuccess` (non-trial, clears one file).
2. Failure: `extendTrialInterval` (trial) vs `feedScopeDetection` (non-trial). Calling scope detection on trial failure would overwrite the doubled interval with a fresh initial interval.
3. Failure-skip: Non-trial handles 403/permission errors; trial skips these (trial failures are always scope-related).

**D9: Retrier hashes entire files synchronously.** `runRetrierSweep` → `createEventFromDB` → `observeLocalFile` → `ComputeStableHash` reads the entire file content to compute a QuickXorHash. For a 10GB file, this blocks for 10+ seconds. The hash is redundant for upload retries because the worker's `UploadFile` (transfer_manager.go:438) hashes the file again during the actual upload. The event loop (or current drain goroutine) does blocking file I/O that produces a value the worker will independently recompute.

**D10: reobserve is unnecessary.** `reobserve` (engine.go:2355, 85 lines) makes a blocking Graph API call (`GetItem`, ~200ms) from the drain goroutine during trial dispatch. Its purpose is to check whether a scope condition (429/507) has cleared before dispatching a full action. But the worker's first HTTP request serves the same purpose — if the scope is still blocked, the worker gets 429/507 in ~200ms (same cost as reobserve's GET). The worker result carries `RetryAfter`. Eliminating reobserve removes 85 lines, a blocking API call from the critical path, and all deduplication concerns around trial timer re-firing before the reobserve completes.

### Objectives

| # | Objective | Measurable outcome | Fixes |
|---|-----------|-------------------|-------|
| O1 | Eliminate admission duplication | One `admit()` function. `admitAndDispatch` and `admitReady` deleted. | D1 |
| O2 | Make ordering invariants compiler-enforced | All 5 ordering constraints are sequential function calls in one goroutine. Cannot break without changing the function body. | D3 |
| O3 | Eliminate cross-goroutine shared mutable state | Zero mutexes and zero atomics in engine state. `trialMu`, `syncErrorsMu`, `watchShortcutsMu`, `permissionCache.mu` all deleted. `succeeded`/`failed`/`nextActionID` become plain ints. | D2, D4 |
| O4 | Eliminate reobserve | `reobserve` function deleted. Trial dispatch uses `createEventFromDB` (cached DB state). Worker execution is the scope test. | D10 |
| O5 | Add mtime fast-path to observeLocalFile | `observeLocalFile` skips hashing when mtime+size match baseline. Shared `BaselineEntry.MatchesStat()` with scanner (DRY). Missing >250GB size guard added. | General improvement |
| O6 | Lazy hash for upload retries | `createEventFromDB` for upload direction skips hashing — sets size/mtime from stat, hash from baseline (if mtime-clean) or empty string. Worker hashes during `UploadFile`. Event loop never blocks on file I/O for retries. | D9 |
| O7 | Fix handle403 redundant boundary walks | `handle403` checks denied prefixes before API call. First 403 walks; subsequent 403s under same prefix are <0.1ms. | D6 |
| O8 | Unify select loops | Single `runEventLoop(ctx, LoopConfig)` with nil channels for mode-specific cases. Three loops → one function. | D7 |
| O9 | Merge result processing | Single `processResult` with `isTrial` branches. `processTrialResult` and `applyResultSideEffects` deleted. | D8 |
| O10 | File split | No file in `internal/sync/` exceeds 800 lines (excluding tests). | D5 |

### Non-Objectives

- **Decompose the Engine struct into smaller types.** The struct stays large. Methods are distributed across files by concern. After the event loop is stable, a follow-up phase can extract testable sub-structs (FailureRecorder, TrialManager, etc.) — but that is separate work, not part of this plan.
- **Change any user-visible behavior.** Pure refactoring. All R-x.y.z requirements remain at their current status.
- **Optimize performance.** Latency is preserved, not improved. The debounce window (2s) dominates all other costs.
- **Async I/O for permission handling.** `handle403` and `recheckPermissions` stay synchronous. Justification in Section 4.
- **Counter semantics for watch mode.** `succeeded`/`failed` semantics are unchanged — they accumulate in watch mode. When R-2.9 (RPC/status) is designed, counter lifecycle can be revisited. The event loop makes any future scheme (per-batch, sliding window, epoch-based) trivially safe because all counter access is single-goroutine.

---

## 2. Root Cause and Approach

### Root Cause

The defects are caused by **two goroutines sharing mutable state**. The main goroutine (`runWatchLoop`) plans batches and dispatches actions. The drain goroutine (`drainWorkerResults`) processes worker results and dispatches dependents. Both need to make admission decisions, access trial state, update shortcuts, record errors, and arm timers. This sharing forces duplication (D1), requires synchronization (D2, D4), and creates implicit ordering constraints (D3).

The alternative of decomposing the Engine struct into smaller types (7 structs connected by callbacks) was evaluated and rejected. It redistributes the shared state into smaller containers but does not eliminate it — each new struct still needs its own mutex. The mutexes are reorganized, not removed. The callbacks add indirection without fixing the root cause.

### Approach

Collapse the two goroutines into a **single event loop**. One goroutine owns all engine state. No mutexes needed — sequential access is guaranteed by the Go memory model. Ordering invariants are trivially correct — they're sequential function calls in the same goroutine. Admission duplication is structurally impossible — there's one goroutine, so there's one admission path.

Workers, observers, the bridge goroutine, the debounce goroutine, and the reconciliation goroutine are unchanged. They communicate with the event loop via channels (readyCh, results, events, reconcileDoneCh). The event loop is the only consumer of engine state.

### What Must Be Preserved

| Design decision | Why it exists | Preserved by |
|----------------|---------------|--------------|
| Actor-with-outbox drain pattern | Prevents readyCh ↔ results deadlock (R-6.8.9) | Same outbox in event loop select |
| Workers never call `depGraph.Complete` | Single authority for completion ordering | Unchanged |
| No held queue in ScopeGate | DB is the queue; crash-safe (R-2.10.5, R-6.5.1) | Unchanged |
| Scope detection ≠ scope admission | ScopeState detects patterns, ScopeGate blocks actions (R-2.10.3) | Unchanged |
| Trial snapshot iteration | `AllDueTrials` returns a snapshot; prevents infinite loops (R-2.10.5) | Unchanged |
| Persistent DepGraph in watch mode | Actions from multiple batches coexist | Unchanged |
| 2-second debounce latency | Balance responsiveness with batching (R-2.1.2) | Buffer unchanged |
| Pipeline overlap | Batch N+1 planned while batch N executes | Event loop interleaves planning and result processing naturally |
| Warm workers | Goroutines always ready, no per-batch infrastructure cost | WorkerPool unchanged |

### What's NOT Wrong

The event-driven concurrent model is correct:
- **2-second latency** for local file changes (fsnotify → debounce → plan → dispatch → worker)
- **Pipeline overlap**: planning and result processing interleave in the event loop
- **Warm workers**: goroutines always ready
- **WebSocket readiness**: when R-2.8.5 lands, remote change events enter the same buffer via `buf.Add()`

---

## 3. Target Architecture

### Current: Two Goroutines + Shared State

```
Main goroutine (runWatchLoop)          Drain goroutine (drainWorkerResults)
├─ 7 select cases                      ├─ 5 select cases (actor-with-outbox)
├─ processBatch → admitAndDispatch     ├─ processWorkerResult → admitReady
├─ handleRecheckTick                   ├─ processTrialResult
├─ runFullReconciliationAsync          ├─ runTrialDispatch → reobserve (blocking API)
├─ recordSkippedItems                  ├─ runRetrierSweep → hash files (blocking I/O)
│                                      ├─ handle403 → walkPermissionBoundary (blocking API)
└─ Shared via mutex/atomic:            └─ Shared via mutex/atomic:
   trialPending (trialMu)                trialPending (trialMu)
   trialTimer/retryTimer (trialMu)       scopeGate (internal mu)
   watchShortcuts (RWMutex)              permCache (RWMutex)
   depGraph (internal mu)                depGraph (internal mu)
   succeeded/failed (atomics)            succeeded/failed (atomics)
   syncErrors (syncErrorsMu)             syncErrors (syncErrorsMu)
```

### Target: One Event Loop

```
                   Bridge goroutine        Debounce goroutine
Observers ──events──► buf.Add() ──notify──► FlushDebounced ──ready──┐
                                                                     │
Workers ──results──────────────────────────────────────────────────┐ │
                                                                   │ │
Reconciliation ──reconcileDoneCh──────────────────────────────┐   │ │
                                                               ↓   ↓ ↓
                                         ┌───────────────────────────────┐
                                         │         Event Loop            │
                                         │     (single goroutine)        │
                                         │                               │
                                         │  ALL engine state:            │
                                         │  ├─ trialPending (plain map)  │
                                         │  ├─ permDenied (plain map)    │
                                         │  ├─ shortcuts (plain slice)   │
                                         │  ├─ syncErrors (plain slice)  │
                                         │  ├─ deleteCounter             │
                                         │  ├─ succeeded (plain int)     │
                                         │  ├─ failed (plain int)        │
                                         │  └─ nextActionID (plain int)  │
                                         │                               │
                                         │  12 select cases:             │
                                         │  ├─ outbox → readyCh          │
                                         │  ├─ worker results            │
                                         │  ├─ ready batch               │
                                         │  ├─ skippedCh                 │
                                         │  ├─ recheckTick               │
                                         │  ├─ reconcileTick             │
                                         │  ├─ reconcileDoneCh           │
                                         │  ├─ observerErrs              │
                                         │  ├─ trialTimer.C              │
                                         │  ├─ retryTimer.C              │
                                         │  ├─ emptyCh (bootstrap only)  │
                                         │  └─ ctx.Done()                │
                                         └────────────┬──────────────────┘
                                                      │
                                           readyCh ◄──┘ (outbox pattern)
                                                      │
                                                Worker Pool
```

### Goroutine Inventory

| Goroutine | Current | Target | Change |
|-----------|---------|--------|--------|
| Main/watch loop | 1 | 0 | **Merged into event loop** |
| Drain loop | 1 | 0 | **Merged into event loop** |
| Event loop | 0 | 1 | **NEW** |
| Workers | N | N | Unchanged |
| Remote observer | 0-1 | 0-1 | Unchanged |
| Local observer | 0-1 | 0-1 | Unchanged |
| Bridge (events → buffer) | 1 | 1 | Unchanged |
| Debounce (buffer → ready) | 1 | 1 | Unchanged |
| Reconciliation | 0-1 | 0-1 | Changed: sends result via `reconcileDoneCh` instead of calling `setShortcuts` directly |
| Timer callbacks | 0-2 | 0 | **Eliminated** (direct `time.Timer.C` in select) |

Net change: main + drain + timer callbacks → event loop. No new goroutine types.

---

## 4. Design Decisions

### Decision 1: No Async I/O for Permissions

**Decision**: `handle403` → `walkPermissionBoundary` and `recheckPermissions` stay synchronous in the event loop. No `asyncResultCh` channel. No async result types.

**Analysis**: After eliminating `reobserve` (O4), only two blocking API calls remain:

| Call | Latency | Frequency | Buffering during stall |
|------|---------|-----------|----------------------|
| `handle403` → `walkPermissionBoundary` | ~1s (1-3 API calls) | Once per denied prefix, cached forever | Results channel: 4096 capacity. 8 workers at 1 result/sec = 8 results during 1s stall. No overflow. |
| `recheckPermissions` | ~200ms × N prefixes | Once per 60s (throttled) | Same buffering. ~600ms stall with 3 denied prefixes. |

**Why synchronous is acceptable**:

1. **`handle403` is a one-time cost per prefix.** After the first 403 triggers a boundary walk, `permDenied[boundary] = false` caches the result forever. The O7 prefix-check fix prevents redundant walks for subsequent 403s under the same prefix within the same pass. On the next pass, the planner reads `deniedPrefixes()` and suppresses uploads under denied paths. Workers never get 403 for those paths again. The ~1s stall happens once per denied prefix in the lifetime of the sync session.

2. **`recheckPermissions` already blocks the main goroutine today.** It's called from `periodicPermRecheck` → `processBatch` in the current `runWatchLoop` (engine.go:3020). Moving it to the event loop changes nothing about the blocking behavior. It's ~600ms once per 60 seconds.

3. **The async alternative has real costs.** An `asyncResultCh` channel requires: 2 new typed result structs (`permCheckResult`, `permWalkResult`), a sealed interface, a type-switch handler in the event loop, deferred state updates (the event loop must update `permDenied` when the async result arrives, not when the 403 was detected), and edge case reasoning for in-flight async results during shutdown. All for 2 calls that together stall <2 seconds per 60 seconds.

4. **The architecture doesn't preclude adding it later.** If profiling shows the stalls matter (e.g., under heavy 403 load with many distinct denied prefixes), async I/O can be added as a follow-up without changing the event loop's structure. The select loop gains one case; the synchronous call becomes a goroutine launch. This is additive, not architectural.

**What stays synchronous** (complete list of event loop blocking calls):

| Operation | Latency | Justification |
|-----------|---------|---------------|
| DB writes (RecordFailure, ClearSyncFailure, etc.) | ~1-2ms | SQLite WAL, always fast |
| DB reads (PickTrialCandidate, ListSyncFailuresForRetry) | ~1-5ms | Indexed queries, small tables |
| `handle403` → `walkPermissionBoundary` | ~1s first per prefix | One-time, cached (O7 prevents redundancy) |
| `recheckPermissions` | ~200ms × N prefixes | Once per 60s, already blocks today |
| `os.Stat` in `isDirAccessible` | ~0.1ms | Local filesystem metadata |
| `planner.Plan()` | ~0.01-1ms | Pure function, in-memory |
| `observeLocalFile` (mtime hit) | ~0.1ms | Stat only, hash skipped (O5) |

**Worst-case event loop stall**: Retry sweep processing 1024 items × ~1ms DB read = ~1 second. Same stall occurs in the current drain goroutine. Batch-limited by `retryBatchSize` with immediate re-arm.

### Decision 2: Eliminate reobserve, Use Worker-as-Probe

**Decision**: Delete `reobserve` entirely. Trial dispatch uses `createEventFromDB` (cached DB state for downloads, `observeLocalFile` for uploads). The worker's first HTTP request serves as the scope probe.

**Why reobserve existed**: To make a lightweight GET call to detect if a scope condition (429/507) cleared before committing to a full action. The idea: don't waste a full upload/download attempt if the scope is still blocked.

**Why it's unnecessary**:

1. **Same cost.** The worker's first HTTP request takes ~200ms — the same as reobserve's GET. If the scope is still blocked, the worker gets 429/507 and returns with `RetryAfter` in the `WorkerResult`. The trial result handler extends the interval and re-arms the timer. Identical outcome.

2. **reobserve creates complexity.** It's 85 lines of code that handles upload/download/delete directions differently, makes a blocking API call from the drain goroutine, and requires careful reasoning about what happens if the trial timer re-fires while a reobserve is in-flight. Eliminating it removes all of these concerns.

3. **The event from `createEventFromDB` flows through the normal pipeline.** Buffer → planner → `admit()` intercepts via `trialPending` → marks `IsTrial=true` → worker executes → `processResult` handles the result. No special path.

**Tradeoffs accepted**:

- For downloads where the item was deleted during a scope block: worker gets 404 (classified as `resultRequeue`), trial interval extends. The item stays in `sync_failures` until a different item's trial clears the scope, or `isFailureResolved` catches it in the retrier. Suboptimal but self-correcting.
- Loss of reobserve's 404-detection for scope clearance: if ALL items in a scope were deleted remotely, no trial can succeed (all get 404). The scope persists until `isFailureResolved` clears all candidates and `PickTrialCandidate` returns nil → `onScopeClear`. Self-healing, but slower than reobserve's direct detection.

### Decision 3: Lazy Hash for Upload Retries

**Decision**: When `createEventFromDB` is called for an upload retry, skip hashing the file. Set hash to empty string (or baseline hash if mtime-clean via O5). The worker hashes during `UploadFile` anyway.

**Analysis**: The retrier sweep calls `createEventFromDB` → `observeLocalFile` → `ComputeStableHash`. This reads the entire file to compute a QuickXorHash. For a 10GB file, this blocks for 10+ seconds. The hash is then used by the planner to compare against `Baseline.LocalHash`:

- If `Local.Hash != Baseline.LocalHash` → planner generates upload → correct for retries
- If `Local.Hash == Baseline.LocalHash` → planner says "no change" → sync_failure cleaned up

The worker's `UploadFile` (transfer_manager.go:438) independently computes the hash via `tm.hashFunc(localPath)` before uploading. So the event loop and the worker both hash the same file — the event loop's hash is redundant.

**What happens with an empty hash**:

1. **Primary case (file modified, upload previously failed):** `"" != baseline.LocalHash` → planner generates upload → worker hashes and uploads → correct.
2. **File synced by another path in the meantime:** O5's mtime fast-path detects this: current mtime matches baseline mtime → reuse `baseline.LocalHash` → planner says "no change" → no action → correct. If mtime doesn't match (edge case), empty hash → planner generates upload → worker uploads → idempotent success → `clearFailureOnSuccess` cleans up.
3. **File deleted:** `observeLocalFile` returns a `ChangeDelete` event (os.ErrNotExist path) → planner generates delete or no-op → correct.

**Implementation**: `observeLocalFile` gains a `skipHash` parameter (or a separate `observeLocalFileFast` variant) used only by the retrier path. The stat is still needed for size/mtime. The hash is omitted or filled from baseline via O5.

### Decision 4: Keep syncdispatch Mutexes

**Decision**: `DepGraph.mu` and `ScopeGate.mu` in the `syncdispatch/` package keep their mutexes. No cross-package API changes.

**Justification**:

1. **Defense-in-depth.** `syncdispatch` is a separate package with its own API. Future callers (tests, CLI tools, other engine modes) might access DepGraph or ScopeGate from multiple goroutines. The mutex cost is ~100ns per call — unmeasurable against the 2-second debounce window.

2. **No cross-package changes.** Removing the mutex would change `syncdispatch`'s thread-safety contract. The mutex is a private field — it doesn't affect the public API. Keeping it is free.

3. **The event loop's single-goroutine guarantee applies to engine state, not external packages.** The engine's `trialPending`, `permDenied`, `shortcuts`, `syncErrors` etc. are engine-internal and benefit from mutex removal. DepGraph and ScopeGate are shared infrastructure with their own safety guarantees.

### Decision 5: reconcileDoneCh for Reconciliation Completion

**Decision**: The reconciliation goroutine sends its result via a `reconcileDoneCh` (buffered 1) channel. The event loop receives it and updates state. No direct cross-goroutine writes.

**Current problem**: `runFullReconciliationAsync` calls `setShortcuts()` directly from the reconciliation goroutine — a cross-goroutine write protected by `watchShortcutsMu`. After the event loop merge, this would be the only remaining cross-goroutine write to engine state.

**Design**:

```go
type reconcileResult struct {
    shortcuts []synctypes.Shortcut
    err       error
}
```

The reconciliation goroutine:
1. Performs full delta enumeration (blocking I/O, correctly off the event loop)
2. Commits observations to DB
3. Feeds events to buffer via `buf.Add()` (thread-safe, buffer has its own mutex)
4. Sends `reconcileResult` via `reconcileDoneCh`

The event loop:
1. Receives `reconcileResult` from select case
2. Updates `e.shortcuts` (no mutex needed — single goroutine)
3. Logs completion

All sends to `reconcileDoneCh` use `select { case ch <- result: case <-ctx.Done(): }` to prevent goroutine leaks on shutdown.

**Why not eventually-consistent**: An alternative is to skip the completion channel and have the event loop refresh shortcuts from DB on the next batch (~2s delay). This creates a window where `handle403` uses stale shortcuts. The window is harmless in practice (shortcuts are discovered during reconciliation, not created), but the explicit completion channel is cleaner and eliminates the staleness entirely for negligible cost (one more select case, one buffered channel).

---

## 5. The Event Loop

### Unified Select Loop (O8)

One `runEventLoop` function handles all three modes (one-shot, bootstrap, steady-state) using nil channels to disable mode-specific cases. This is the same pattern the current codebase uses: `retryTimerChan()` returns nil in one-shot mode, which blocks forever in select — effectively removing the case.

```go
// LoopConfig holds mode-specific channels. Nil channels disable their select case.
type LoopConfig struct {
    results       <-chan synctypes.WorkerResult  // always set
    bl            *synctypes.Baseline            // always set
    mode          synctypes.SyncMode             // always set
    safety        *synctypes.SafetyConfig        // always set

    // Nil to disable:
    ready         <-chan []synctypes.PathChanges  // batch from buffer (steady-state)
    skippedCh     <-chan []synctypes.SkippedItem  // safety scan results (steady-state)
    errs          <-chan error                    // observer errors (steady-state)
    recheckTick   <-chan time.Time                // permission recheck (steady-state)
    reconcileTick <-chan time.Time                // periodic reconciliation (steady-state)
    emptyCh       <-chan struct{}                 // quiescence signal (bootstrap only)
}

func (e *Engine) runEventLoop(ctx context.Context, c *LoopConfig) error {
    var outbox []*synctypes.TrackedAction

    for {
        var outCh chan<- *synctypes.TrackedAction
        var outVal *synctypes.TrackedAction
        if len(outbox) > 0 {
            outCh = e.readyCh
            outVal = outbox[0]
        }

        select {
        // ── Core (all modes) ──

        case outCh <- outVal:
            outbox = outbox[1:]

        case r, ok := <-c.results:
            if !ok { return nil }
            dispatched := e.processResult(ctx, &r, c.bl)
            outbox = append(outbox, dispatched...)

        case <-ctx.Done():
            return nil

        // ── Watch + bootstrap (nil in one-shot) ──

        case <-e.trialTimer.C:
            e.runTrialDispatch(ctx)

        case <-e.retryTimer.C:
            e.runRetrierSweep(ctx)

        // ── Watch steady-state only (nil in bootstrap and one-shot) ──

        case batch, ok := <-c.ready:
            if !ok { return nil }
            dispatched := e.processBatch(ctx, batch, c.bl, c.mode, c.safety)
            outbox = append(outbox, dispatched...)

        case skipped := <-c.skippedCh:
            e.recordSkippedItems(ctx, skipped)
            e.clearResolvedSkippedItems(ctx, skipped)

        case <-c.recheckTick:
            e.handleRecheckTick(ctx)

        case <-c.reconcileTick:
            e.startReconciliation(ctx, c.bl)

        case rc := <-e.reconcileDoneCh:
            e.finishReconciliation(ctx, rc)

        case obsErr := <-c.errs:
            if e.handleObserverError(obsErr, c) {
                return nil
            }

        // ── Bootstrap only (nil in steady-state and one-shot) ──

        case <-c.emptyCh:
            return nil
        }
    }
}
```

**Mode configurations**:

| Channel | One-shot | Bootstrap | Steady-state |
|---------|----------|-----------|--------------|
| `results` | set | set | set |
| `trialTimer.C` | nil (no timer) | set | set |
| `retryTimer.C` | nil (no timer) | set | set |
| `ready` | nil | nil | set |
| `skippedCh` | nil | nil | set |
| `errs` | nil | nil | set |
| `recheckTick` | nil | nil | set |
| `reconcileTick` | nil | nil | set |
| `reconcileDoneCh` | nil | nil | set (buffered 1) |
| `emptyCh` | nil | set | nil |

### Unified Admission (O1)

One function, called from exactly two places:
1. `processBatch` → `dispatchActions` → `admit` (new actions from observation)
2. `processResult` → `admit` (dependent actions from `depGraph.Complete`)

Both happen in the event loop goroutine. Both return `[]*TrackedAction` for the outbox.

```go
func (e *Engine) admit(ctx context.Context, ready []*synctypes.TrackedAction) []*synctypes.TrackedAction {
    var dispatch []*synctypes.TrackedAction

    for _, ta := range ready {
        // Trial interception — NO LOCK (single goroutine)
        if entry, isTrial := e.trialPending[ta.Action.Path]; isTrial {
            delete(e.trialPending, ta.Action.Path)

            if entry.scopeKey.BlocksAction(ta.Action.Path, ta.Action.ShortcutKey(),
                ta.Action.Type, ta.Action.TargetsOwnDrive()) {
                ta.IsTrial = true
                ta.TrialScopeKey = entry.scopeKey
                dispatch = append(dispatch, ta)
            } else {
                // Stale trial: scope no longer blocks this action type.
                // Clear the failure and fall through to normal admission.
                e.store.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID)
                if key := e.scopeGate.Admit(ta); key.IsZero() {
                    e.setDispatch(ctx, &ta.Action)
                    dispatch = append(dispatch, ta)
                }
                e.armTrialTimer()
            }
            continue
        }

        // Normal scope gate admission — NO LOCK (single goroutine)
        if e.scopeGate != nil {
            if key := e.scopeGate.Admit(ta); !key.IsZero() {
                e.cascadeRecordAndComplete(ctx, ta, key)
                continue
            }
        }

        e.setDispatch(ctx, &ta.Action)
        dispatch = append(dispatch, ta)
    }

    return dispatch
}
```

This single function includes the stale-trial cleanup path from `admitReady` — the more complete version subsumes the simpler one. The stale-trial path exists because a scope block might be cleared for certain action types while a trial is pending — the trial's scope key no longer blocks the action, so the failure should be cleared and the action admitted normally.

### Unified Result Processing (O9)

```go
func (e *Engine) processResult(ctx context.Context, r *synctypes.WorkerResult,
    bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
    class, _ := classifyResult(r)
    ready, _ := e.depGraph.Complete(r.ActionID)
    isTrial := r.IsTrial && !r.TrialScopeKey.IsZero()

    // ── Dependent routing (identical for trial and non-trial) ──
    var dispatched []*synctypes.TrackedAction
    switch class {
    case resultSuccess:
        dispatched = e.admit(ctx, ready)
    case resultShutdown:
        e.completeSubtree(ready)
        return nil
    default:
        e.cascadeFailAndComplete(ctx, ready, r)
    }

    // ── Side effects ──
    switch class {
    case resultSuccess:
        e.succeeded++
        if isTrial {
            e.onScopeClear(ctx, r.TrialScopeKey)
        } else {
            e.clearFailureOnSuccess(ctx, r)
        }
        e.scopeState.RecordSuccess(r)

    case resultShutdown:
        // no side effects

    default:
        if isTrial {
            // Extend trial interval. Do NOT call feedScopeDetection — scope
            // is already blocked; re-detecting would overwrite the doubled
            // interval with a fresh initial interval (A2 bug prevention).
            e.extendTrialInterval(r.TrialScopeKey, r.RetryAfter)
        } else {
            e.feedScopeDetection(r)
            if class == resultSkip {
                if errors.Is(r.Err, os.ErrPermission) {
                    e.handleLocalPermission(ctx, r)
                    e.recordError(r)
                    return dispatched
                }
                if r.HTTPStatus == http.StatusForbidden && e.permHandler != nil {
                    e.handle403(ctx, bl, r)
                }
            }
        }

        var delayFn func(int) time.Duration
        if class == resultRequeue || class == resultScopeBlock {
            delayFn = retry.Reconcile.Delay
        }
        e.recordFailure(ctx, r, delayFn)
        e.recordError(r)
        e.armRetryTimer()
    }

    return dispatched
}
```

The 3 `isTrial` branches:
1. **Success**: `onScopeClear` (releases entire scope, admits all blocked items) vs `clearFailureOnSuccess` (clears one file's failure). Trial success means the scope condition resolved; non-trial success means one file synced.
2. **Failure**: `extendTrialInterval` vs `feedScopeDetection`. Scope detection on trial failure would overwrite the doubled interval with a fresh initial interval — the documented "A2 bug."
3. **Failure-skip (403)**: Non-trial handles 403/permission errors via `handle403`; trial skips these because trial failures are always scope-related (429/507), never permission-related (403).

### Trial Dispatch Without reobserve (O4)

```go
func (e *Engine) runTrialDispatch(ctx context.Context) {
    now := e.nowFunc()
    e.cleanStaleTrialPending(now)

    for _, key := range e.scopeGate.AllDueTrials(now) {
        row, err := e.store.PickTrialCandidate(ctx, key)
        if err != nil { continue }
        if row == nil {
            e.onScopeClear(ctx, key)
            continue
        }

        e.trialPending[row.Path] = trialEntry{scopeKey: key, created: now}

        ev := e.createEventFromDB(ctx, row) // DB-cached state, no API call
        if ev == nil {
            e.onScopeClear(ctx, key)
            continue
        }
        e.buf.Add(ev)
    }
    e.armTrialTimer()
}
```

The event flows through the normal pipeline: buffer → debounce → planner → `admit()` intercepts via `trialPending` → marks `IsTrial=true` → worker executes → `processResult` handles the result with `isTrial` branches. If worker gets 429/507 → `extendTrialInterval`. If worker succeeds → `onScopeClear`.

### handle403 Prefix-Check Fix (O7)

```go
func (e *Engine) handle403(ctx context.Context, bl *synctypes.Baseline, r *synctypes.WorkerResult) {
    failedPath := r.Path

    // Fast path: already under a known denied prefix — skip API call (O7)
    for _, prefix := range e.deniedPrefixes() {
        if failedPath == prefix || strings.HasPrefix(failedPath, prefix+"/") {
            return // already handled, failure recorded by caller
        }
    }

    // ... existing: find shortcut, query API, walk boundary, cache result ...
}
```

After this fix, the stall pattern per denied prefix is:
- **1st 403**: boundary walk (~1s). Cache `permDenied[boundary] = false`.
- **2nd-8th 403** (same prefix, same pass): prefix check (<0.1ms each).
- **Next pass**: planner reads `deniedPrefixes()`, suppresses uploads under that prefix. No 403s reach workers.

### Timer Management

```go
func (e *Engine) armTrialTimer() {
    earliest, ok := e.scopeGate.EarliestTrialAt()
    if !ok {
        e.trialTimer.Stop()
        return
    }
    delay := time.Until(earliest)
    if delay <= 0 {
        delay = time.Millisecond
    }
    // Canonical drain-before-reset pattern (no mutex needed — single goroutine)
    if !e.trialTimer.Stop() {
        select {
        case <-e.trialTimer.C:
        default:
        }
    }
    e.trialTimer.Reset(delay)
}
```

The timer's `.C` channel appears directly in the event loop select. No goroutine, no signal channel, no mutex. `retryTimer` uses the same pattern with its own `*time.Timer`. The semantically-wrong sharing of `trialMu` between trial and retry timers becomes structurally impossible — each timer is a plain field, accessed only by the event loop goroutine.

**Eliminated**: `trialCh` channel, `retryTimerCh` channel, `trialMu` mutex, 0-2 `time.AfterFunc` callback goroutines.

### Reconciliation

```go
func (e *Engine) startReconciliation(ctx context.Context, bl *synctypes.Baseline) {
    if !e.reconcileRunning.CompareAndSwap(false, true) {
        return
    }

    go func() {
        defer e.reconcileRunning.Store(false)

        events, shortcuts, err := e.performReconciliation(ctx, bl)

        select {
        case e.reconcileDoneCh <- reconcileResult{
            events:    events,
            shortcuts: shortcuts,
            err:       err,
        }:
        case <-ctx.Done():
        }
    }()
}

func (e *Engine) finishReconciliation(ctx context.Context, rc reconcileResult) {
    if rc.err != nil { return }

    e.shortcuts = rc.shortcuts

    filtered := filterOutShortcuts(rc.events)
    for i := range filtered {
        e.buf.Add(&filtered[i])
    }
}
```

### Bootstrap

Bootstrap uses `runEventLoop` with a `LoopConfig` that has `emptyCh` set and observer channels nil:

```go
func (e *Engine) bootstrap(ctx context.Context, results <-chan synctypes.WorkerResult,
    mode synctypes.SyncMode, safety *synctypes.SafetyConfig,
) error {
    bl, err := e.loadWatchState(ctx)
    if err != nil { return err }

    // Observation and permission rechecks — no event loop yet, safe to block
    e.recheckLocalPermissions(ctx)
    if e.permHandler != nil {
        e.recheckPermissions(ctx, bl, e.shortcuts)
    }

    changes, err := e.observeChanges(ctx, bl, mode)
    if err != nil { return err }
    if len(changes) == 0 { return nil }

    plan, err := e.planner.Plan(changes, bl, mode, safety, e.deniedPrefixes())
    if err != nil { return err }

    dispatched := e.dispatchAndAdmit(ctx, plan)

    return e.runEventLoop(ctx, &LoopConfig{
        results: results,
        bl:      bl,
        mode:    mode,
        safety:  safety,
        emptyCh: e.depGraph.WaitForEmpty(),
        // ready, errs, tickers all nil — observers not started
    })
}
```

### RunOnce / RunWatch Duality

```
RunOnce:
  1. verifyDriveIdentity, ResetInProgressStates
  2. Load baseline, observe changes, plan
  3. Create DepGraph, readyCh, WorkerPool (ephemeral)
  4. Dispatch actions, admit ready
  5. Start workers
  6. Run runEventLoop with one-shot LoopConfig (3 active select cases)
  7. Stop workers, wait for results channel close
  8. Return report

RunWatch:
  1. Create all engine state (timers, reconcileDoneCh, etc.)
  2. Start worker pool (persistent)
  3. Bootstrap:
     a. Observe + plan + dispatch
     b. Run runEventLoop with bootstrap LoopConfig (emptyCh set)
     c. Exit when DepGraph empties
  4. Start observers
  5. Run runEventLoop with steady-state LoopConfig (12 active select cases)
  6. On shutdown: stop workers, wait for drain
```

---

## 6. State Ownership After Refactoring

### Engine State (Single Goroutine — No Synchronization)

| State | Type | Previously | After |
|-------|------|-----------|-------|
| `trialPending` | `map[string]trialEntry` | `trialMu` Mutex | Plain map |
| `trialTimer` | `*time.Timer` | `trialMu` Mutex | Plain field, `.C` in select |
| `retryTimer` | `*time.Timer` | `trialMu` Mutex (wrong sharing) | Plain field, `.C` in select |
| `shortcuts` | `[]synctypes.Shortcut` | `watchShortcutsMu` RWMutex | Plain slice |
| `syncErrors` | `[]error` | `syncErrorsMu` Mutex | Plain slice |
| `permDenied` | `map[string]bool` | `permissionCache.mu` RWMutex | Plain map (replaces `permissionCache` struct) |
| `succeeded` | `int` | `atomic.Int32` | Plain int |
| `failed` | `int` | `atomic.Int32` | Plain int |
| `nextActionID` | `int64` | `atomic.Int64` | Plain int64 |
| `deleteCounter` | `*syncdispatch.DeleteCounter` | Engine-internal | Unchanged |
| `scopeGate` | `*syncdispatch.ScopeGate` | Engine-internal | Unchanged (keeps internal mutex) |
| `scopeState` | `*syncdispatch.ScopeState` | Engine-internal | Unchanged |
| `depGraph` | `*syncdispatch.DepGraph` | Engine-internal | Unchanged (keeps internal mutex) |

**Totals eliminated**: 4 mutexes (`trialMu`, `watchShortcutsMu`, `syncErrorsMu`, `permissionCache.mu`), 4 atomics (`succeeded`, `failed`, `nextActionID`, `reconcileRunning` stays), 2 signal channels (`trialCh`, `retryTimerCh`), 1 struct (`permissionCache` → plain map).

### Cross-Goroutine State (Channels Only)

| State | Shared with | Protection |
|-------|-------------|------------|
| `readyCh` | Workers | Channel semantics |
| `results` | Workers | Channel semantics |
| `reconcileDoneCh` | Reconciliation goroutine | Buffered(1) channel, ctx-guarded sends |
| `events` | Observers, bridge | Channel semantics |
| `Buffer.mu` | Bridge, debounce | Buffer's internal mutex |
| `reconcileRunning` | Reconciliation goroutine | `atomic.Bool` (CAS guard) |
| `SyncStore` | Workers, observers, reconciliation, CLI | SQLite WAL + busy_timeout |

All cross-goroutine communication uses channels or the database. Zero shared mutable memory between the event loop and other goroutines (except the reconciliation CAS guard).

---

## 7. Critical Ordering Dependencies

All 5 ordering invariants are trivially preserved because everything runs sequentially in one goroutine:

1. **Cascade record BEFORE returning dependents**: `cascadeRecordAndComplete` calls `recordScopeBlockedFailure` then `depGraph.Complete` — sequential in the same function body. Go guarantees statement ordering. No interleaving possible.

2. **Trial fallback must clear stale failure BEFORE normal admission**: `ClearSyncFailure` then `scopeGate.Admit` — sequential in `admit()`. Same function, same goroutine.

3. **`onScopeClear` BEFORE admitting dependents**: Sequential in `processResult`. `onScopeClear` runs, then `admit` runs on the ready list.

4. **Scope detection NOT called for trial failures**: `if isTrial { extendTrialInterval } else { feedScopeDetection }` in `processResult`. The branch is explicit and commented with the A2 bug rationale. Cannot be accidentally reordered.

5. **`retryBatchSize` limits event loop stall**: Same batch limiting in `runRetrierSweep`. The event loop processes the batch, returns to select, re-fires immediately for the next batch via re-arm.

---

## 8. File Split

### Target File Layout

After the event loop refactoring, `internal/sync/` production files:

| File | Contents | ~Lines |
|------|----------|--------|
| `engine.go` | Engine struct, NewEngine, Close, RunOnce, RunWatch, LoopConfig | ~600 |
| `engine_event_loop.go` | runEventLoop, processResult, admit, processBatch | ~400 |
| `engine_observe.go` | observeChanges, observeRemote, observeLocal, observeLocalFile, createEventFromDB, observeAndCommit variants | ~400 |
| `engine_scope.go` | scope detection, scope blocks, trial dispatch, trial intervals, timer arming | ~400 |
| `engine_retry.go` | retrier sweep, isFailureResolved, failure recording, cascading | ~400 |
| `engine_watch.go` | initWatchInfra, bootstrap, startObservers, startReconciliation, finishReconciliation | ~500 |
| `engine_record.go` | recordFailure, recordError, logFailureSummary, recordSkippedItems, clearResolvedSkippedItems | ~400 |
| `engine_shortcuts.go` | shortcut processing, registration, reconciliation (already separate) | ~600 |
| `engine_util.go` | delete counter, external changes, helpers, resolvers | ~300 |
| `permissions.go` | deniedPrefixes (plain map), findShortcutForPath, isDirAccessible, helpers | ~150 |
| `permission_handler.go` | handle403, walkPermissionBoundary, recheckPermissions (already separate) | ~existing |
| `orchestrator.go` | Orchestrator, DriveRunner (unchanged) | ~450 |
| `drive_runner.go` | DriveRunner (unchanged) | ~40 |
| `types.go` | Engine-internal types (trialEntry, resultClass, etc.) | ~existing |

No production file exceeds 800 lines. Methods stay on `*Engine` — the file split is organizational, not structural.

---

## 9. Migration Roadmap

### Guiding Principles

1. Never break the build. Every commit compiles, passes tests, passes lint.
2. Never break behavior. Every commit preserves all R-x.y.z requirements.
3. TDD throughout. New behavior gets tests before implementation.
4. No backwards compatibility. No migration shims. No re-exports. No `// removed` comments. After each phase, it's as if the old code never existed.
5. Design doc updated per phase.

### Phase 1: File Split (~1 PR)

Split `engine.go` into files by concern per the layout in Section 8. Zero semantic change. All methods stay on `*Engine`. Move code, update imports, verify tests.

**Verification**: `go test -race ./...`, `golangci-lint run`, no file >800 lines.

### Phase 2: Preparatory Refactoring (~2-3 PRs)

These changes work in the current two-goroutine architecture. They reduce the blast radius of Phase 3.

**Phase 2a: Eliminate reobserve (O4).**
- Replace `runTrialDispatch`'s `reobserve` call with `createEventFromDB`.
- Delete `reobserve` function entirely.
- Update `processTrialResult` to handle the case where the trial event flows through the normal pipeline.
- Delete `reobserve` — no remnants.

**Phase 2b: Add mtime optimization + lazy hash (O5, O6).**
- Extract `BaselineEntry.MatchesStat(size, mtime, now) bool` to `synctypes/`.
- Refactor scanner's `classifyFileChange` to use `MatchesStat` (DRY).
- Add mtime fast-path to `observeLocalFile`: when mtime+size match baseline and the mtime is older than 1 second (racily-clean guard), reuse `baseline.LocalHash` without hashing.
- Add the missing >250GB size guard to `observeLocalFile`.
- For the retrier upload path: when mtime doesn't match (file was modified), skip hashing — set hash to empty string. The planner sees `"" != baseline.LocalHash` → generates upload. The worker hashes during `UploadFile`.

**Phase 2c: Fix handle403 prefix check (O7).**
- Add denied-prefix check at the top of `handle403` before any API call.
- Write test: second 403 under same prefix skips boundary walk.

**Phase 2d: Merge processTrialResult into processWorkerResult (O9).**
- Merge `processTrialResult`, `applyResultSideEffects`, and `processWorkerResult` into a single `processResult` with `isTrial` branches.
- Delete the separate functions — no remnants.

### Phase 3: Event Loop Merge (~2-3 PRs)

This is the core change. The highest-risk phase.

**Phase 3a: Merge select loops + unify admission.**

In a single commit:
- Create `LoopConfig` struct and `runEventLoop` function.
- `processBatch` returns `[]*TrackedAction` instead of sending to `readyCh` via `admitAndDispatch`.
- `admitReady` renamed to `admit`, returns `[]*TrackedAction`.
- `admitAndDispatch` deleted.
- `handleTrialInterception` logic folded into `admit`.
- Three select loops replaced by single `runEventLoop`.
- `reconcileDoneCh` (buffered 1) added. `runFullReconciliationAsync` sends result via channel. `finishReconciliation` updates shortcuts in event loop.
- `waitForQuiescence` deleted — bootstrap uses `LoopConfig.emptyCh`.
- `drainWorkerResults` deleted.
- `runWatchLoop` deleted.
- Main goroutine calls `runEventLoop` and blocks on its completion.

**Transition verification**: Full test suite with `-race`. Add targeted stress test: 1000 actions across 10 batches with varying worker completion rates, concurrent with trial timer and retry timer.

**Phase 3b: Remove synchronization primitives.**

With the event loop established, remove primitives one at a time, each in its own commit:

1. `trialMu` → delete. `trialPending` becomes plain `map[string]trialEntry`. `trialTimer`/`retryTimer` become plain `*time.Timer` fields.
2. `watchShortcutsMu` → delete. `watchShortcuts` becomes plain `[]Shortcut` field (rename to `shortcuts`).
3. `syncErrorsMu` → delete. `syncErrors` becomes plain `[]error` field.
4. `succeeded`/`failed` atomics → plain `int` fields. Direct `++` instead of `.Add(1)`.
5. `nextActionID` atomic → plain `int64`. Direct `++` instead of `.Add(1)`.
6. `permissionCache` struct → plain `map[string]bool` field `permDenied`. Delete `permissionCache` type, `newPermissionCache`, `get`/`set`/`reset`/`deniedPrefixes` methods. Replace with direct map access.
7. Timer `time.AfterFunc` + signal channels → direct `time.Timer`. Delete `trialCh`, `retryTimerCh`, `trialTimerChan()`, `retryTimerChan()`, `handleTrialTimer()`, `handleRetryTimer()`.

Each commit must pass `-race`.

### Phase 4: RunOnce Alignment (~1 PR)

Refactor `RunOnce`'s `executePlan` to use `runEventLoop` with a one-shot `LoopConfig`. This makes both modes use the same event loop. Delete `executePlan`'s separate drain goroutine spawn.

### Phase 5: Final Cleanup (~1 PR)

- Remove `watchState` struct entirely. Fields that survive move to `Engine` (immutable config) or become event-loop-scoped locals.
- Delete `watchPipeline` struct — replaced by `LoopConfig`.
- Delete all dead code: `admitAndDispatch`, `admitReady`, `handleTrialInterception`, `processTrialResult`, `applyResultSideEffects`, `drainWorkerResults`, `runWatchLoop`, `waitForQuiescence`, `reobserve`, `trialTimerChan`, `retryTimerChan`, `handleTrialTimer`, `handleRetryTimer`, `resultStats` (if counter access is direct), `setShortcuts`/`getShortcuts` (if replaced by direct field access).
- Delete `permissionCache` type and file if empty after removal.
- Grep for remnants: `trialMu`, `watchShortcutsMu`, `syncErrorsMu`, `permissionCache`, `admitAndDispatch`, `admitReady`, `drainWorkerResults`, `runWatchLoop`, `reobserve`, `AfterFunc`, `retryTimerCh`, `trialCh`. Zero results.
- Update `sync-engine.md` design doc:
  - Delete "watchState" section
  - New "Event Loop" section: merged select, LoopConfig, admission, outbox pattern
  - New "State Ownership" section: every field, which goroutine owns it
  - Update "Scope Detection and Management": simplified for single goroutine
  - "Drain-loop retrier" → "Retrier (event loop case)"
  - "Trial dispatch" → "Trial dispatch (event loop case)"
  - All R-x.y.z Implements lines unchanged
- Update `sync-execution.md`: note DepGraph/ScopeGate keep internal mutexes, single-goroutine access from engine
- Update `sync-observation.md`: note permissionCache replaced by plain map

### Phase Summary

| Phase | PRs | Risk | What's eliminated |
|-------|-----|------|------------------|
| 1: File split | 1 | None | Nothing (organizational) |
| 2a: Eliminate reobserve | 1 | Low | `reobserve` (85 lines), blocking API call in drain |
| 2b: Mtime + lazy hash | 1 | Low | Redundant file hashing in retrier |
| 2c: handle403 prefix check | 1 | Low | Redundant boundary walks |
| 2d: Merge processResult | 1 | Low | `processTrialResult`, `applyResultSideEffects` |
| 3a: Event loop merge | 1 | **HIGH** | Two goroutines → one. `admitAndDispatch`, `admitReady`, `handleTrialInterception`, `drainWorkerResults`, `runWatchLoop`, `waitForQuiescence` |
| 3b: Remove sync primitives | 7 commits | Medium | 4 mutexes, 4 atomics, 2 signal channels, `permissionCache` type |
| 4: RunOnce alignment | 1 | Low | Separate one-shot drain goroutine |
| 5: Final cleanup | 1 | Low | `watchState`, `watchPipeline`, all dead code, design doc update |

Total: ~8-10 PRs. Each independently verifiable with `-race` and E2E.

---

## 10. Test Strategy

### Test Migration Effort

Based on analysis of the existing test suite:

**engine_test.go** (4,903 lines, ~119 tests):
- ~90% (107 tests) use `RunOnce`/`RunWatch` public API — unchanged. These test behavior through the full pipeline, not internal goroutine topology.
- ~7% (8 tests) call `processWorkerResult` directly — mechanical rename to `processResult`.
- ~2% (3 tests) call `processTrialResult` directly — change to call `processResult` with `IsTrial: true`.
- ~1% (1 test) calls `drainWorkerResults` directly — rewrite to use `runEventLoop` with a `LoopConfig`.
- `startDrainLoop` test helper (17 lines) — rewrite to set up a `LoopConfig` instead of spawning a drain goroutine.

**engine_phase4_test.go** (1,641 lines, ~43 tests):
- 2 calls to `admitReady` directly — rename to `admit`.
- 10 uses of `newTestEngine` — likely unchanged (same factory).

**Estimated effort**: ~200 lines of changes across ~15 tests, plus rewriting one 17-line helper.

### New Tests

| Test | Purpose |
|------|---------|
| `TestEventLoop_ProcessBatchAndResult` | Event loop processes batch then result in sequence |
| `TestEventLoop_AdmissionUnified` | Single `admit()` handles trial interception + scope gate + stale cleanup |
| `TestEventLoop_TrialViaWorker` | Trial dispatch → `createEventFromDB` → worker → `processResult` (isTrial) |
| `TestEventLoop_Bootstrap` | Bootstrap dispatches and waits for quiescence via `LoopConfig.emptyCh` |
| `TestEventLoop_ReconciliationCompletion` | `reconcileDoneCh` updates shortcuts in event loop |
| `TestEventLoop_ReconciliationShutdown` | ctx-guarded send prevents goroutine leak |
| `TestEventLoop_OneShotConfig` | `LoopConfig` with nil channels disables watch-mode cases |
| `TestEventLoop_Handle403PrefixCheck` | O7: second 403 under same prefix skips boundary walk |
| `TestProcessResult_TrialBranches` | Merged `processResult` isTrial branches (A2 bug prevention) |
| `TestBaselineEntry_MatchesStat` | Shared mtime+size+racily-clean check (DRY with scanner) |
| `TestObserveLocalFile_LazyHash` | Upload retry path skips hashing, returns empty hash |
| `TestEventLoop_Stress` | 1000 actions, 10 batches, varying rates, `-race` |

### Existing Tests That Need Updating

| Test file | Change |
|-----------|--------|
| `engine_test.go` | Rename `processWorkerResult` → `processResult`, `processTrialResult` → `processResult` with IsTrial. Rewrite `startDrainLoop` helper. |
| `engine_phase4_test.go` | Rename `admitReady` → `admit`. |
| `permissions_test.go` | Replace `permissionCache` usage with plain map access. |

### E2E Tests

Unchanged. The ultimate regression gate.

---

## 11. Risk Analysis

### Risk 1: Goroutine Topology Change (HIGH)

**What could go wrong**: Merging two goroutines changes scheduling behavior. A subtle ordering bug during migration could cause deadlock or data loss.

**Why it's the highest risk**: This is a fundamental change to how work is scheduled. The current architecture has two concurrent select loops; the target has one sequential loop.

**Mitigation**:
- Phase 2 (preparatory refactoring) is zero-topology-change. It simplifies the code without touching goroutine structure. If Phase 3 must be reverted, Phase 2's improvements stand.
- Phase 3a (merge select loops) is one commit, fully testable with `-race`.
- The event loop's sequential model is easier to verify post-migration: feed events in tests, check state sequentially, no race conditions by construction.
- Targeted stress test: concurrent batches + results + timers at high throughput.
- Full E2E suite catches logic bugs that unit tests might miss.

### Risk 2: Pipeline Overlap Regression

**What could go wrong**: Currently, the main goroutine plans batch N+1 while the drain goroutine processes batch N's results. In the event loop, these are serialized in the select.

**Why it's not a problem**: The planner is a pure function (~0.01-1ms for typical batches). Result processing is ~1ms per result. The debounce window is 2000ms. Even with 100 results between batches, processing takes ~100ms — 5% of the debounce window. The serialization is invisible at the timescales involved.

### Risk 3: Timer Reset Gotcha

**What could go wrong**: `time.Timer.Reset` has a documented footgun — you must drain `.C` before calling `Reset` if the timer already fired.

**Mitigation**: Use the canonical drain-before-reset pattern (shown in Section 5, Timer Management). Single goroutine makes this straightforward — no concurrent reads of `.C`.

### Risk 4: Event Loop Stall From DB Operations

**What could go wrong**: SQLite `busy_timeout` (5 seconds) could block the event loop if another connection holds a write lock.

**Mitigation**: SQLite WAL mode allows concurrent reads and a single writer. The engine is the primary writer. The CLI opens a separate connection for queries. Contention is minimal (~1ms). The 5-second busy_timeout is a safety net, not a normal code path.

### Risk 5: Select Priority Starvation

**What could go wrong**: Go's `select` chooses randomly among ready cases. If `results` and `ready` are both constantly firing, one might be starved.

**Why it's acceptable**: Batches arrive every 2 seconds (debounce). Worker results arrive at ~8/second peak. Timers fire every seconds-to-minutes. Simultaneous readiness is infrequent. If profiling shows P99 result-processing latency exceeding 100ms, add an inner drain loop (drain pending results before returning to outer select). Don't pre-optimize.

---

## 12. Verification Checklist

After each phase and at the end:

1. `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. `golangci-lint run`
3. `go build ./...`
4. `go test -race -coverprofile=/tmp/cover.out ./...`
5. `go tool cover -func=/tmp/cover.out | grep total` — coverage must not decrease
6. `go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...` — E2E unchanged
7. All R-x.y.z requirements remain at their current status
8. Design docs updated with correct GOVERNS and Implements lines
9. No signs of old architecture:
   - `grep -r "admitAndDispatch\|admitReady\|handleTrialInterception" internal/sync/` → zero results
   - `grep -r "drainWorkerResults\|runWatchLoop\|waitForQuiescence" internal/sync/` → zero results
   - `grep -r "trialMu\|watchShortcutsMu\|syncErrorsMu\|permissionCache" internal/sync/` → zero results
   - `grep -r "trialCh\|retryTimerCh\|trialTimerChan\|retryTimerChan" internal/sync/` → zero results
   - `grep -r "reobserve" internal/sync/` → zero results
   - `grep -r "processTrialResult\|applyResultSideEffects" internal/sync/` → zero results
   - `grep -r "watchState" internal/sync/` → zero results (struct deleted)
   - `grep -r "watchPipeline" internal/sync/` → zero results (struct deleted)
10. `handle403` has prefix-check short-circuit (O7)
11. `reconcileDoneCh` sends all use `select { case ch <- result: case <-ctx.Done(): }`
12. No production file in `internal/sync/` exceeds 800 lines

---

## 13. Success Criteria

When complete:

1. **One event loop** — single goroutine owns all engine state
2. **One admission path** — `admit()` function, structurally impossible to duplicate
3. **4 engine mutexes eliminated** — `trialMu`, `watchShortcutsMu`, `syncErrorsMu`, `permissionCache.mu` deleted
4. **4 engine atomics eliminated** — `succeeded`, `failed`, `nextActionID` become plain fields. `reconcileRunning` remains (reconciliation goroutine CAS guard)
5. **2 signal channels eliminated** — `trialCh`, `retryTimerCh` deleted. Direct `time.Timer.C` in select
6. **`reobserve` eliminated** — function deleted. Trial dispatch uses `createEventFromDB`. Worker execution is the scope test
7. **`observeLocalFile` has mtime fast-path** — unchanged files skip hashing. DRY via `BaselineEntry.MatchesStat`
8. **Retrier upload path uses lazy hash** — event loop never blocks on file I/O for upload retries
9. **`handle403` prefix check** — redundant boundary walks eliminated
10. **One `runEventLoop` function** — three modes via `LoopConfig` with nil channels
11. **One `processResult` function** — trial/non-trial merged with `isTrial` branches
12. **`reconcileDoneCh` with ctx guard** — no goroutine leaks on shutdown, no stale shortcuts
13. **`syncdispatch` mutexes preserved** — defense-in-depth, no cross-package changes
14. **All existing tests pass** — zero regressions
15. **E2E tests pass** — behavior identical
16. **Design docs accurate** — GOVERNS lines match, Implements statuses unchanged
17. **No file >800 lines** in `internal/sync/` (excluding test files)
18. **Ordering invariants trivially correct** — sequential execution in single goroutine
19. **Zero remnants of old architecture** — grep verification passes (Section 12, item 9)

---

## 14. Follow-Up Work (Not Part of This Plan)

### Component Extraction

After the event loop is stable and proven in E2E, extracting sub-structs becomes trivially safe — plain structs, no mutexes, no callbacks, just method calls from the event loop:

- `FailureRecorder` — needs: SyncStore, driveID. ~14 methods. Testable with mock store alone.
- `TrialManager` — needs: ScopeGate, SyncStore, Buffer. Trial dispatch + stale cleanup.
- `RetryManager` — needs: SyncStore, DepGraph, Buffer. Retry sweep + resolution.
- `PermissionManager` — needs: SyncStore, PermissionChecker, ScopeGate. Already partly separate in `permission_handler.go`.
- `DeleteProtection` — needs: SyncStore. Rolling counter + held-delete recording.

This is future work, not committed. The event loop guarantees single-goroutine access, making extraction safe whenever it's prioritized.

### Counter Semantics for Watch Mode

`succeeded`/`failed` accumulate monotonically in watch mode. When R-2.9 (RPC/status) is designed, counter lifecycle should be revisited. The event loop makes any scheme (per-batch reset, sliding window, epoch-based) trivially safe — all counter access is single-goroutine.

### WebSocket Support (R-2.8.5)

When webhook/WebSocket events arrive, they enter the same pipeline as observer events: `buf.Add(event)`. The event loop processes them identically — debounced batch → planner → dispatch. No architectural change needed. A WebSocket observer would be another goroutine calling `buf.Add()`, same as the remote observer today.

---

## Appendix A: Design Provenance

This plan synthesizes ideas from two prior proposals: "Plan C" (event loop, synchronous I/O) and "Plan D" (event loop with async I/O and component extraction). Both plans were independently developed, then reviewed, challenged, and revised through multiple rounds. This section documents every design point from both plans — what was adopted, what was rejected, and why.

### Points of Agreement (Adopted From Both)

Both plans independently converged on the same core architecture. These points were adopted without controversy:

| Design point | Plan C | Plan D | Status |
|-------------|--------|--------|--------|
| Single event loop replacing main + drain goroutines | Core thesis | Core thesis | **Adopted** |
| Unified `admit()` function eliminating D1 duplication | O1 | O-1 | **Adopted** |
| Direct `time.Timer.C` in select replacing `AfterFunc` + signal channels | Section 7 | Section 8 | **Adopted** |
| Ordering invariants become trivially correct | O3 | O-3 | **Adopted** |
| `LoopConfig` with nil channels for mode-specific cases | O8 | Sections 4, 11 | **Adopted** |
| File split as Phase 1 | Phase 1 | Phase 1 | **Adopted** |
| `processBatch` returns `[]*TrackedAction` instead of sending to `readyCh` | Section 3 | Section 4 | **Adopted** |
| Actor-with-outbox pattern preserved in event loop | Section 3 | Section 4 | **Adopted** |
| `watchState` struct eliminated, fields on Engine or loop-scoped | Phase 5 | Phase 4 | **Adopted** |

### Plan C Ideas Adopted

| Design point | Justification |
|-------------|---------------|
| **Eliminate reobserve (O4)** | Plan C's key insight. `reobserve` makes a ~200ms blocking API call that the worker duplicates. The worker's first HTTP request IS the scope probe. Eliminating reobserve removes 85 lines, a blocking API call, and all async-reobserve edge cases. Plan D originally kept reobserve (making it async), then adopted this approach in revision. |
| **Mtime fast-path for observeLocalFile (O5)** | Plan C identified that `observeLocalFile` unconditionally hashes files with no mtime check and no >250GB size guard. The scanner already has both optimizations. O5 extracts `BaselineEntry.MatchesStat()` (DRY) and adds the fast-path. Plan D had no equivalent — it addressed only the retrier hash case (D-9) but not the general `observeLocalFile` path. |
| **handle403 prefix-check fix (O7)** | Plan C identified that `handle403` goes straight to API calls without checking the denied-prefix cache. The fix is surgical: check the cache first, skip the walk if the path is already under a known denied prefix. Plan D did not identify this gap. |
| **Synchronous handle403 / recheckPermissions (Decision 1)** | Plan C argued that after eliminating reobserve, the remaining blocking calls (handle403 once per prefix, recheckPermissions once per 60s) are rare and bounded. The async pattern for 2 calls adds a channel, 2 types, a type switch, and deferred state updates. Plan D's async I/O infrastructure was over-engineering for the remaining call sites. |
| **reconcileDoneCh (Decision 5)** | Plan C proposed a buffered(1) channel for reconciliation completion, allowing the event loop to update shortcuts immediately. Plan D originally had eventually-consistent shortcuts (2s stale window), then adopted reconcileDoneCh in revision. |
| **Merge processTrialResult into processResult (O9)** | Plan C's unified result processing with `isTrial` branches. ~130 lines of duplicated routing → ~50 lines with inline branches. Plan D had the same idea but structured it differently (early return for trials). The inline-branch approach is cleaner. |
| **`DepGraph.WaitForEmpty` for bootstrap quiescence** | Plan C used `LoopConfig.emptyCh` set to `depGraph.WaitForEmpty()` for the bootstrap select case, eliminating `waitForQuiescence` as a separate function. Cleaner than maintaining a separate bootstrap loop. |

### Plan D Ideas Adopted

| Design point | Justification |
|-------------|---------------|
| **Lazy hash for upload retries (D-9 / O6)** | Plan D identified that the retrier's `createEventFromDB` → `observeLocalFile` → `ComputeStableHash` reads entire files, and the worker's `UploadFile` hashes the same file again during upload. The event loop hash is redundant. Plan C's mtime fast-path (O5) does NOT fix this case — for a modified file awaiting upload retry, mtime won't match baseline, so the hash is still computed. Plan D's lazy hash is the correct fix for the retrier-specific case. Both optimizations are adopted: O5 for general mtime optimization, O6 for retrier upload retries. |
| **Granular phasing (Phases 2a-2f)** | Plan D's revised migration (goroutine merge first as 2a, then independent follow-up phases) is better than combining the merge with other changes. Each phase is independently testable. If the merge introduces a bug, you know it's the topology change, not an unrelated optimization. Plan C's phasing was less granular. |
| **Keep syncdispatch mutexes (Decision 4)** | Plan D (in revision) correctly argued that DepGraph.mu and ScopeGate.mu should stay. They're private fields in an external package with ~100ns overhead. Defense-in-depth against future callers. Plan C's original Phase 3c proposed removing them, which would require cross-package API changes. |
| **Component extraction as future work** | Plan D proposed extracting FailureRecorder, TrialManager, RetryManager, PermissionManager, DeleteProtection as Phase 5 sub-structs. Plan C originally called this a "Non-Objective." The truth is in between: extraction becomes trivially safe after the event loop (no mutexes needed), but it's separate work that should be decided independently. Listed in Follow-Up Work (Section 14) with Plan D's specific component list. |
| **Counter semantics identified as gap (D-7 / O-7)** | Plan D identified that `succeeded`/`failed` accumulate monotonically in watch mode and nobody reads them. Plan C didn't address this. The fix (per-batch counters) is deferred because R-2.9 has no design doc yet, but Plan D's identification of the problem is acknowledged. Listed in Follow-Up Work (Section 14). |
| **WebSocket readiness (O-11)** | Plan D explicitly called out WebSocket readiness as an objective. The architecture naturally supports it (events enter via `buf.Add()`), but Plan D's framing made this explicit. Adopted in Follow-Up Work (Section 14). |

### Plan D Ideas Rejected

| Design point | Reason for rejection |
|-------------|---------------------|
| **Mandatory async I/O via `asyncResultCh`** | Plan D's central thesis: "The event loop must never block on API calls. This is not optional." After eliminating reobserve (which Plan D later adopted), only 2 blocking calls remain: `handle403` (~1s, once per denied prefix, cached forever) and `recheckPermissions` (~600ms, once per 60s, already blocks the main goroutine today). The async pattern requires a new channel, 2 typed result structs, a sealed interface, a type-switch handler, and deferred state updates. This is significant complexity for 2 rare, bounded stalls. The "never block" principle sounds clean but is violated by both plans anyway — SQLite writes, `os.Stat`, `planner.Plan()`, and (until O6) retrier hash computation all block. The question is "acceptable blocking," not "zero blocking." The event loop architecture doesn't preclude adding async I/O later if profiling shows a problem. |
| **`asyncResult` interface with 3 typed implementations** | Eliminated by rejecting async I/O. The interface pattern (`trialReobserveResult`, `permCheckResult`, `permWalkResult`) was well-designed but unnecessary — `trialReobserveResult` was eliminated by O4 (reobserve deletion), and the remaining 2 types serve the rejected async permission pattern. |
| **Combined goroutine merge + async I/O wiring (original Phase 2b)** | Plan D originally proposed combining the topology change with async I/O wiring in a single PR, arguing it was "safer than doing them separately." This was reversed in revision after challenge — the combined change is larger and harder to debug. If the merge introduces a bug, you can't tell if it's from the topology change or the async wiring. Plan D's revised phasing (merge first, then follow-up) is adopted. |
| **Eventually-consistent shortcut refresh (original reconciliation design)** | Plan D originally had the reconciliation goroutine feed events to buffer without explicit completion signaling. The event loop refreshed shortcuts from DB on the next batch (~2s delay). Replaced by reconcileDoneCh (Plan C's design), which Plan D adopted in revision. |
| **Removing syncdispatch mutexes (original Phase 2d/2f)** | Plan D originally proposed removing DepGraph.mu and ScopeGate.mu after the event loop merge. Reversed in revision — they're private fields in an external package, defense-in-depth is worth keeping, and removing them requires cross-package API changes for negligible benefit. |
| **Counter semantics fix as part of this plan (O-7)** | Plan D's per-batch counter reset is a real improvement, but R-2.9 (RPC/status endpoint) has no design doc. Designing counter semantics now without knowing what the RPC endpoint needs is premature. Deferred to Follow-Up Work. The event loop makes any future scheme trivially safe. |
| **Component extraction as committed Phase 5** | Plan D committed to 3-4 PRs of sub-struct extraction. This is good work but adds significant scope to an already-large refactoring. Keeping it as "future work, not committed" lets us evaluate after the event loop is proven. The extraction is ENABLED by the event loop (single-goroutine access = no mutexes needed), but it's not REQUIRED by it. |
| **Benchmarking latency impact (O-9 verification)** | Plan D proposed benchmarking "time from buffer flush to action dispatch" before and after. The event loop serializes planning and result processing that currently run in separate goroutines, but `planner.Plan()` takes <1ms for typical batches — invisible against the 2s debounce. A benchmark would show noise, not signal. If a real performance regression appears, it'll show in E2E latency, not microbenchmarks. |

### Points Where Both Plans Were Wrong

| Issue | What both plans said | What the code shows |
|-------|---------------------|-------------------|
| **"7 mutexes"** | Both claimed 7 mutexes protecting engine state. | Actual count: 6 in engine + syncdispatch (trialMu, watchShortcutsMu, syncErrorsMu, permCache.mu, DepGraph.mu, ScopeGate.mu). The 7th (Buffer.mu) is external to both goroutines. |
| **"admitReady has stale cleanup that admitAndDispatch lacks"** | Both framed this as a missing path. | `admitAndDispatch` delegates to `handleTrialInterception` (engine.go:1609), which handles stale trials through a different code path. The cleanup isn't missing — it's different. The duplication is real; the framing slightly exaggerated the gap. |
| **DepGraph/ScopeGate "in engine.go"** | Both originally referenced DepGraph/ScopeGate as if they were in engine.go. | They've been extracted to `syncdispatch/` in a prior refactoring. Both plans were corrected in revision. |
| **"retryTimer sharing trialMu is wrong" (D2)** | Both called this "wrong." | It works. It's a code smell (unrelated concepts sharing a lock), but not a bug. Both timers are only accessed under short critical sections, and both are stopped together in `stopTrialTimer`. The sharing is accidental rather than intentional, which makes it a maintenance risk, not a correctness bug. The event loop eliminates the shared lock regardless. |
