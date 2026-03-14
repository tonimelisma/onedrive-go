# Scope & Tracker Redesign

**Status**: Proposed
**Scope**: `internal/sync/tracker.go`, `internal/sync/engine.go`, `internal/sync/scope.go`, `internal/sync/planner.go`, `internal/sync/worker.go`, `internal/sync/engine_shortcuts.go`, `internal/sync/reconciler.go`, `internal/sync/store_failures.go`, `internal/sync/store_admin.go`
**Governs**: All files currently governed by sync-execution.md's Tracker and Executor sections

---

## 1. Problem Statement

The sync engine's action dispatch system has eleven correctness, safety, and maintainability defects that trace to a single architectural flaw: `DepTracker` mixes dependency graph management, scope-based admission control, and channel dispatch in one type. Additionally, the held queue loses work on crash, the failure retrier runs as a disconnected goroutine with sparse fake events, the shortcut scope pipeline is non-functional, and scope-blocked sync_failures can become orphaned when the underlying condition resolves through the normal pipeline.

### 1.1 Defects

**D-1: Data race in `dispatch()`.** `dispatch()` reads `dt.scopeBlocks` and writes `dt.held` — both maps. Called under `dt.mu` from `Add()` (tracker.go:157), but WITHOUT `dt.mu` from `Complete()` (tracker.go:205, lock released at line 199) and `ReleaseScope()` (tracker.go:365). In watch mode, `Add` runs on the main goroutine while `Complete` runs on the drain goroutine (`go e.drainWorkerResults`, engine.go:993). Concurrent map access. Undefined behavior per Go spec.

**D-2: Lock ordering fragility.** `onHeld` callback (tracker.go:86-90) calls `armTrialTimer()` which acquires `trialMu`. To prevent `dt.mu → trialMu` ordering, `onHeld` is called after releasing `dt.mu`. This constraint is documented in a comment, not enforced by the type system.

**D-3: `DiscardScope` orphans dependents.** `DiscardScope` (tracker.go:379-400) increments `completed` but doesn't decrement dependents' `depsLeft`, doesn't remove from `dt.actions`, doesn't clean `dt.byPath`. Second terminal path that bypasses the dependency graph.

**D-4: Channel-send-under-lock in `Add()`.** `Add()` calls `dispatch()` under `dt.mu`. `dispatch()` may block on `dt.ready <- ta`. Workers can't call `Complete` (needs `dt.mu`). Deadlock. Avoided only by buffer sizing.

**D-5: Shortcut scope pipeline non-functional.** `Action.targetShortcutKey` and `Action.targetDriveID` are never populated. `remoteStateFromEvent()` (planner.go:619-633) discards `RemoteDriveID` and `RemoteItemID` from `ChangeEvent`. The entire shortcut scope blocking system is inoperative.

**D-6: Dual sync_failures clearing.** `updateRemoteStateOnOutcome()` (store_baseline.go:449-540) deletes from sync_failures inside the CommitOutcome transaction. `defensiveClearFailure()` (engine.go:1489-1505) deletes again after receiving the result. Dual ownership masks bugs.

**D-7: Held actions dispatched with stale state.** `ReleaseScope` dispatches held actions directly to workers with PathViews from planning time. In watch mode, held actions may be 30+ minutes stale. `CancelByPath` can't cancel held actions (`ta.Cancel` is nil — set by workers, but held actions never reached a worker). Stale `remote_state` entries ("downloading"/"deleting") are never cleaned up.

**D-8: Held queue lost on crash.** Held actions are in-memory only. Process crash → all held work is lost. `ResetInProgressStates` cleans up remote_state on restart but doesn't know about actions that were in the held queue.

**D-9: FailureRetrier uses sparse fake events.** `synthesizeFailureEvent()` (reconciler.go:218-246) creates ChangeEvents with most fields empty (no hash, size, mtime, etag, ctag, name; ItemType hardcoded to file). The planner makes decisions on incomplete PathViews. Runs as a separate goroutine with its own dedup map (`dispatchedRetryAt`), disconnected from the engine's drain loop.

**D-10: `Complete()` does not delete from `dt.actions`.** `Complete()` (tracker.go:196-199) deletes from `dt.byPath` but NOT from `dt.actions`. Completed actions linger in the map. In the new design where scope-blocked actions are immediately completed, a subsequent `Add()` for a new action could find the completed action's ID in `dt.actions`, wire a dependency edge to it, and the dependent waits forever (nobody will decrement its `depsLeft` — the action is already completed). This is a prerequisite for the DepGraph extraction — `Complete` must `delete(dt.actions, id)` after copying dependents.

**D-11: Orphaned sync_failures when underlying condition resolves.** When a scope-blocked action's sync_failure is created and the underlying condition later resolves through the normal pipeline (e.g., remote file deleted during hold → delta poll observes deletion → planner produces no action), nobody clears the sync_failure. It sits with `next_retry_at = NULL` (invisible to retrier) until the scope clears, at which point the retrier re-processes it pointlessly. Without a staleness check, orphaned sync_failures are retried forever.

### 1.2 Root Cause

`DepTracker` mixes three concerns:

| Concern | Synchronization needs |
|---|---|
| **Dependency graph** — track actions, manage dep edges, process dependents | Mutex for map access. No channel ops. |
| **Scope gate** — hold actions when API is throttled/blocked | Mutex for scope block maps. No channel ops. |
| **Channel dispatch** — feed the worker pool | Blocking channel send. Cannot be under any mutex. |

These interact at `dispatch()` (tracker.go:272-282). Every defect traces to this function or to the held queue it manages.

---

## 2. Action State Machine

**This state machine is the authoritative reference for the action lifecycle. The governing design specs (sync-execution.md, sync-engine.md) MUST document it in full. Any code change that modifies states or transitions MUST update the design spec first.**

### 2.1 States

| State | Storage | Description |
|---|---|---|
| **OBSERVED** | In-memory (ChangeEvent) | Observer detected a change (delta poll, fsnotify, or retrier re-observation). |
| **PLANNED** | In-memory (Action) | Planner created the action from a PathView. |
| **DISPATCHED** | Database (`remote_state.sync_status`) | Engine called `setDispatch()`. Downloads → "downloading", local deletes → "deleting". Uploads/folder creates/moves have no dispatch-time persistent state. |
| **GRAPH_ADDED** | In-memory (`depGraph.actions[id]`) | Action in the dependency graph. May be waiting on deps (`depsLeft > 0`) or ready (`depsLeft == 0`). |
| **SCOPE_BLOCKED** | Database (`sync_failures` with `scope_key`) | Action's deps satisfied but scope gate blocked it. Recorded as sync_failure. Graph entry completed immediately. |
| **READY** | In-memory (`readyCh` buffer) | Passed scope gate. Waiting for worker. |
| **EXECUTING** | In-memory (worker goroutine) | Worker pulled from `readyCh`, executing. |
| **COMMITTED** | Database (baseline updated) | Worker called `CommitOutcome` on success. |
| **TERMINAL** | In-memory (`completed` incremented) | Engine called `depGraph.Complete()`. Action removed from graph. |

### 2.2 Transitions

```
OBSERVED (ChangeEvent from observer or retrier)
    │
    ▼
PLANNED (Action + PathView from planner)
    │
    │ setDispatch (remote_state for downloads/deletes)
    ▼
DISPATCHED
    │
    │ depGraph.Add()
    ▼
GRAPH_ADDED
    │
    ├── depsLeft > 0 → waits (parent Complete → depsLeft--)
    │                       │
    │                       ▼ (depsLeft == 0)
    │                  returned to engine
    │
    ├── depsLeft == 0 → returned to engine
    │
    ▼
scopeGate.Admit()
    │
    ├── blocked → SCOPE_BLOCKED
    │                 │
    │                 │ engine records sync_failure (scope_key set,
    │                 │   next_retry_at = NULL)
    │                 │ engine resets dispatch status
    │                 │ depGraph.Complete → dependents cascade
    │                 │
    │                 │ ... scope clears (trial succeeds) ...
    │                 │
    │                 │ SetScopeRetryAtNow → retrier picks up
    │                 │ retrier checks isFailureResolved → skip or re-inject
    │                 │ re-inject: event from DB state → buffer → planner
    │                 ▼
    │             OBSERVED (from retrier, using DB state)
    │
    └── not blocked → READY (readyCh)
                          │
                          │ worker pulls
                          ▼
                      EXECUTING
                          │
                          ├── success → COMMITTED → TERMINAL
                          │     engine calls Complete
                          │     dependents returned → routeReadyActions (drain goroutine)
                          │
                          └── failure → TERMINAL
                                engine records sync_failure
                                engine calls Complete
                                dependents: cascade record + complete
                                    (failure-aware: don't dispatch
                                     children of failed parents)
```

### 2.3 Scope Blocking Is Watch-Mode Only

Scope detection (`ScopeState`, `feedScopeDetection`) is only initialized in `RunWatch` (engine.go:1004). In one-shot mode, `scopeState` is nil. `feedScopeDetection` is a no-op. No scope blocks are created. The scope gate never blocks. All actions pass through to workers.

In one-shot mode, 429/507 responses are classified as `resultScopeBlock`, recorded as sync_failures with backoff, and the action is completed. The item sits in sync_failures until the next `onedrive sync` invocation. This is correct — one-shot has no trial mechanism.

---

## 3. Design

### 3.1 DepGraph — Pure Dependency Graph

No scope awareness. No channels. No callbacks.

```go
type DepGraph struct {
    mu         sync.Mutex
    actions    map[int64]*TrackedAction
    byPath     map[string]*TrackedAction
    total      atomic.Int32
    completed  atomic.Int32
    done       chan struct{}
    persistent bool
    logger     *slog.Logger
}

func (g *DepGraph) Add(action *Action, id int64, depIDs []int64) *TrackedAction
func (g *DepGraph) Complete(id int64) []*TrackedAction
func (g *DepGraph) HasInFlight(path string) bool
func (g *DepGraph) CancelByPath(path string)
func (g *DepGraph) InFlightCount() int
func (g *DepGraph) Done() <-chan struct{}
```

`Add` returns the action if immediately ready (deps satisfied), nil otherwise. `Complete` returns all dependents that became ready, deletes the completed action from `actions` and `byPath`. Both return data — no channel sends, no callbacks, no external calls under the lock.

**Critical fix (D-10)**: `Complete` MUST `delete(dt.actions, id)` after copying dependents. The current code only deletes from `byPath`. Without this, a completed action lingers in the map. In the new design where scope-blocked actions are immediately completed, a subsequent `Add` could find the completed action, wire a dependency edge to it, and the dependent waits forever.

**Fixes D-1** (no `dispatch()`, no map access outside lock), **D-4** (no channel sends under lock), **D-10** (`Complete` cleans up `actions` map).

### 3.2 ScopeGate — Scope Blocks + Trial Metadata

No held queue. No dependency awareness. No channels.

```go
type ScopeGate struct {
    mu     sync.Mutex
    blocks map[ScopeKey]*ScopeBlock
    store  ScopeBlockStore  // persists to scope_blocks table
    logger *slog.Logger
}

func (g *ScopeGate) Admit(ta *TrackedAction) ScopeKey
func (g *ScopeGate) SetScopeBlock(key ScopeKey, block *ScopeBlock)
func (g *ScopeGate) ClearScopeBlock(key ScopeKey)
func (g *ScopeGate) IsScopeBlocked(key ScopeKey) bool
func (g *ScopeGate) NextDueTrial(now time.Time) (ScopeKey, time.Time, bool)
func (g *ScopeGate) EarliestTrialAt() (time.Time, bool)
func (g *ScopeGate) GetScopeBlock(key ScopeKey) (ScopeBlock, bool)
func (g *ScopeGate) ExtendTrialInterval(key ScopeKey, nextAt time.Time, newInterval time.Duration)
func (g *ScopeGate) ScopeBlockKeys() []ScopeKey
func (g *ScopeGate) LoadFromStore(ctx context.Context) error  // startup
```

`Admit` checks if the action matches any active scope block. Returns the blocking key or zero. Does NOT hold the action — the caller (engine) records it as a sync_failure and completes it.

`SetScopeBlock`, `ClearScopeBlock`, `ExtendTrialInterval` persist to the `scope_blocks` table. In-memory map is the cache; database is the source of truth.

`NextDueTrial` no longer checks held queue length — if a scope block exists with non-zero `NextTrialAt`, a trial is due. If `PickTrialCandidate` returns nil (no sync_failures for this scope), the engine clears the scope block.

**Fixes D-2** (no `onHeld` callback — `armTrialTimer` called inline by engine), **D-8** (scope blocks persisted, survive crash).

### 3.3 Persisted Scope Blocks

```sql
CREATE TABLE scope_blocks (
    scope_key      TEXT PRIMARY KEY,
    issue_type     TEXT NOT NULL,
    blocked_at     INTEGER NOT NULL,     -- unix nanos
    trial_interval INTEGER NOT NULL,     -- nanoseconds
    next_trial_at  INTEGER NOT NULL,     -- unix nanos
    trial_count    INTEGER NOT NULL DEFAULT 0
);
-- No FK between scope_blocks and sync_failures. Intentional.
-- sync_failures.scope_key is a denormalized lookup key for grouping
-- items by scope (ListSyncFailuresByScope, DeleteSyncFailuresByScope).
-- sync_failures rows must survive scope_blocks deletion — onScopeClear
-- queries them AFTER deleting the scope_blocks row. A CASCADE DELETE
-- would destroy them. Per-item failures may also have empty scope_key
-- (not scope-related at all).
```

Typically 0-5 rows. Mutations: insert on scope detection, update on trial interval extension, delete on scope clear.

On startup: `LoadFromStore` reads all rows into the in-memory map. Trial timer arms from `EarliestTrialAt`. The engine resumes exactly where it left off — no crash recovery gap.

### 3.4 Scope-Blocked Actions → sync_failures

When `Admit` returns a blocking key, the engine records the action as a sync_failure and completes it in the graph immediately. No held queue. Everything is persistent.

```go
// In processBatch / executePlan (MAIN goroutine), after depGraph.Add returns a ready action:
// Uses admitAndDispatch — no trial interception (trialPending is drain-goroutine-only state).
e.admitAndDispatch(ctx, []*TrackedAction{ta})
```

`cascadeRecordAndComplete` is a BFS that records each action (and all its transitive dependents) as sync_failures and completes them in the graph:

```go
func (e *Engine) cascadeRecordAndComplete(ctx context.Context,
    ta *TrackedAction, scopeKey ScopeKey) {

    seen := make(map[int64]bool)
    queue := []*TrackedAction{ta}

    for len(queue) > 0 {
        current := queue[0]
        queue = queue[1:]
        if seen[current.ID] {
            continue
        }
        seen[current.ID] = true

        e.recordScopeBlockedFailure(ctx, &current.Action, scopeKey)
        e.resetDispatchStatus(ctx, &current.Action)
        ready := e.depGraph.Complete(current.ID)
        queue = append(queue, ready...)
    }
}
```

ALL dependents are cascade-completed, regardless of their own scope status. A download (not blocked by quota) that depends on a scope-blocked folder create must not dispatch — the folder create never executed, the local directory doesn't exist. The dependents inherit the parent's scope_key in their sync_failure records.

**Scope key inheritance and multi-scope interaction**: A dependent inherits the parent's scope_key, even if the dependent would ALSO be blocked by a different scope on its own merits. When the parent's scope clears, the dependent is re-processed, re-planned, dispatched — and if a second scope is active, the scope gate blocks it on the second pass. It enters `cascadeRecordAndComplete` again with the second scope's key. This is correct and self-resolving — each pass discovers the next blocking scope.

`recordScopeBlockedFailure` writes to sync_failures with `next_retry_at = NULL` (SQL NULL). The retrier's `ListSyncFailuresForRetry` uses `WHERE next_retry_at <= ?` — in SQLite, `NULL <= ?` evaluates to NULL, which is falsy. NULL rows are never returned. The retrier is completely unaware of scope-blocked items. Only `onScopeClear` (section 3.7) makes them retryable.

**Fixes D-3** (no `DiscardScope` — `Complete` is the single terminal path), **D-7** (no stale dispatch — blocked actions are never dispatched), **D-8** (everything in sync_failures, crash-safe).

### 3.5 Failure-Aware Dependent Dispatch

When a worker result comes back, the engine decides what to do with dependents based on the parent's outcome:

```go
ready := e.depGraph.Complete(r.ActionID)

if r.Success {
    e.routeReadyActions(ready)
} else {
    // Parent failed — children would fail too (e.g., folder doesn't exist).
    // Record each as sync_failure with exponential backoff and complete.
    for _, dep := range ready {
        e.recordFailure(ctx, depToWorkerResult(dep, r), retry.Reconcile.Delay)
        e.depGraph.Complete(dep.ID)
    }
}
```

Cascade-failed dependents get `next_retry_at` set by `retry.Reconcile.Delay` (exponential backoff), NOT `NULL`. They are independently retriable by the retrier. When the retrier picks up a cascade-failed child (e.g., an upload whose parent folder create failed), `createEventFromDB` calls `observeLocal` → the planner builds a PathView → discovers the parent folder doesn't exist in the baseline → creates BOTH a folder create AND the upload with the dependency edge. The planner re-discovers the full dependency tree from current state.

The parent has its own sync_failure entry (recorded by `processWorkerResult` when the worker failed). The children have separate entries (from cascade). Each has independent `next_retry_at` with exponential backoff. The retrier processes them as separate items. When the retrier picks up a child, the planner re-discovers the parent dependency and creates both actions with the dependency edge — the parent folder create is re-attempted, then the child upload. When the retrier picks up the parent, the planner creates the parent action. If both land in the same retrier batch, the planner batch has both paths → both actions created with the correct dependency edge → one execution of each. No duplication — the planner deduplicates by path (PathChanges merge), `HasInFlight` prevents double-dispatch, and the dependency graph ensures ordering. `setDispatch` only writes remote_state for downloads and local deletes, NOT for folder creates — so no stale dispatch status concern for folder create retries.

This prevents N children each making doomed API calls when a folder create fails.

### 3.6 Scope Clear — No Inline Re-Observation

When a trial succeeds, `onScopeClear` does NOT re-observe items inline. Inline re-observation would block the drain loop (100k GetItem calls = minutes of stall) and could re-trigger the rate limit that just cleared. Instead, it makes the sync_failures retryable and lets the retrier handle re-processing at its own pace:

```go
func (e *Engine) onScopeClear(ctx context.Context, key ScopeKey) {
    e.scopeGate.ClearScopeBlock(key) // persists: deletes from scope_blocks table
    e.baseline.SetScopeRetryAtNow(ctx, key) // sets next_retry_at = NOW for all items in scope
    e.armRetryTimer() // retrier picks them up on next sweep
}
```

The retrier (section 3.8) handles the rest: batch-limited sweeps, stale-check per item, event creation from DB state, injection into the planner pipeline.

**Why no re-observation is needed for scope-cleared items**: During the hold period, the normal observation pipeline keeps running. Delta polls update remote_state. fsnotify observes local changes. Non-blocked actions for the same paths may succeed and clear the sync_failure via `clearFailureOnSuccess`. By the time the scope clears, the DB state (remote_state, baseline) is fresh from continuous delta polling. Workers discover current state during execution: uploads read files from disk (`UploadFile` takes `localPath`, executor_transfer.go:91), downloads fetch current content from the API (`DownloadToFile` uses ItemID, executor_transfer.go:37). Stale PathViews don't cause incorrect sync outcomes — they may cause a few unnecessary actions (the planner over-plans), but the executor handles this safely.

**Scope-type-specific analysis of why DB state is sufficient:**

| Scope | Blocks | DB state during hold | Reobservation needed? |
|---|---|---|---|
| disk:local | Downloads | remote_state fresh (delta polls continue) | No |
| quota:own | Own-drive uploads | Worker reads local file at execution time | No |
| quota:shortcut | Shortcut uploads | Worker reads local file at execution time | No |
| throttle:account | Everything | Possibly stale (delta also 429'd) — see note below | No |
| service | Everything | Same as throttle:account | No |
| perm:dir | Directory subtree | remote_state fresh (delta continues). fsnotify may miss local changes during hold — periodic reconciliation catches within 30 min. | No (reconciliation handles) |

**Global scope staleness gap (throttle:account, service)**: During a global scope hold, delta polls are also blocked (429/5xx on the delta endpoint). The remote observer retries with exponential backoff. remote_state may become stale. When the scope clears (`SetScopeRetryAtNow`), the retrier may process items before the next delta poll refreshes remote_state.

This causes a small number of wasted API calls:
- Downloads for items deleted on remote → worker gets 404 → per-item failure → `isFailureResolved` catches it on next sweep (remote_state updated by delta poll that ran in the meantime)
- Actions based on stale hashes/sizes → worker discovers current state during execution (downloads fetch by ItemID, uploads read from disk)

This does NOT cause incorrect sync outcomes. Workers always use current state: `UploadFile` reads the local file, `DownloadToFile` fetches by ItemID from the API. The stale remote_state only affects the planner's action classification, not the worker's execution.

The gap self-corrects: the remote observer's next poll (within its backoff interval, typically 30-120 seconds after the scope clears) refreshes remote_state for ALL changed items. On the next retrier sweep, `isFailureResolved` catches items whose remote_state now shows "deleted" or "synced." Items that genuinely need re-syncing are re-processed with fresh DB state.

**Future improvement**: Add a `ForcePoll` channel to the remote observer so `onScopeClear` can signal an immediate delta poll for global scopes. Combined with a retrier check that skips items whose scope was cleared more recently than the last delta poll, this would eliminate the staleness gap entirely. Not required for launch — the self-correction is fast and the wasted calls are few.

**In-flight trial race**: If a trial is in-flight (worker executing) and a different mechanism clears the scope (e.g., `handleExternalChanges` detects permission fix), `onScopeClear` runs. When the trial result arrives, `processTrialResult` calls `onScopeClear` again. The second `SetScopeRetryAtNow` is a no-op or harmless re-SET. No guard needed.

When a shortcut is deleted:

```go
e.scopeGate.ClearScopeBlock(scopeKey)
e.baseline.DeleteSyncFailuresByScope(ctx, scopeKey) // nothing to re-process
```

### 3.7 `reobserve` — Re-Observation for Trials Only

`reobserve` fetches the current state of a single item with a real API call (remote) or filesystem stat (local). It is used **only for trials** where we need definitive fresh state to test whether a scope has recovered. It is NOT used for bulk re-processing (scope clears, retrier sweeps) — those use DB state.

```go
// reobserve fetches the current state of a single item and returns a
// complete ChangeEvent. Returns nil on unrecoverable error.
//
// Remote actions (download, delete): GetItem(driveID, itemID).
//   200 → full ChangeEvent with hash, size, mtime, etag, name, item_type.
//   404 → ChangeDelete event.
//   429/507/5xx → nil (scope/service still blocked).
//
// Local actions (upload): os.Stat + ComputeQuickXorHash.
//   File exists → full ChangeEvent with hash, size, mtime.
//   File gone → ChangeDelete event.
//   Error → nil.
func (e *Engine) reobserve(ctx context.Context, path string,
    itemID string, driveID driveid.ID, actionType ActionType) *ChangeEvent
```

**Note on remote_state schema**: remote_state contains hash, size, mtime, etag, item_type, path, parent_id, drive_id, item_id. It does NOT contain `name` (derivable from path via `filepath.Base`) or `ctag` (not persisted; not used by the planner for action classification). Events created from remote_state are nearly complete — sufficient for planner decisions.

### 3.8 Retrier Integrated into Drain Loop

The separate FailureRetrier goroutine is eliminated. Its sweep logic becomes a timer in the drain loop with batch limiting and stale-item detection:

```go
case <-retryTimerCh:
    rows := e.baseline.ListSyncFailuresForRetry(ctx, now, e.retryBatchSize)
    //                                                     ^^^^^^^^^^^^^^^
    //                         LIMIT in SQL query — paces the sweep
    for _, row := range rows {
        if e.depGraph.HasInFlight(row.Path) {
            continue
        }

        // D-11: Check if this failure is stale — the underlying condition
        // may have resolved through the normal pipeline (delta poll observed
        // deletion, another action succeeded for this path, etc.)
        if e.isFailureResolved(ctx, &row) {
            _ = e.baseline.ClearSyncFailure(ctx, row.Path, row.DriveID)
            continue
        }

        ev := e.createEventFromDB(ctx, &row)
        if ev != nil {
            e.buf.Add(ev)
        }
    }
    if len(rows) == e.retryBatchSize {
        e.armRetryTimer() // more items — sweep again soon
    } else {
        e.armRetryTimerFromDB() // normal: arm for earliest next_retry_at
    }
```

**Batch limiting**: `retryBatchSize` limits how many items are processed per sweep (e.g., `2 × transferWorkers`). This prevents drain loop stalls. Workers consume the batch, retrier fires again, processes the next batch. After a scope clear with 100k items, the retrier feeds the pipeline in steady batches — workers are always busy, the drain loop isn't stalled.

**Pacing**: When the batch is full (`len(rows) == retryBatchSize`), `armRetryTimer()` re-arms with **zero delay** — the timer fires on the next drain loop iteration. This means back-to-back sweeps when there are items due, giving maximum throughput. Each sweep is bounded by `retryBatchSize`, so the drain loop processes one batch, handles any pending worker results or trial timers in the select, then immediately sweeps the next batch. When the batch is NOT full (`len(rows) < retryBatchSize`), `armRetryTimerFromDB()` queries `EarliestSyncFailureRetryAt` and arms the timer for that future time — normal idle behavior.

**Recovery pace after scope clear**: 100k items at `retryBatchSize = 32`: each sweep processes 32 items (~milliseconds for DB reads + `observeLocal` stat/hash), injects into buffer, drain loop returns to select. Workers pull from readyCh and execute. The retrier is NOT the bottleneck — worker throughput is (8 workers × ~2s/item = ~4 items/sec). The retrier keeps readyCh full; workers drain it at their pace. Total recovery: 100k / 8 workers / ~2s = ~7 hours, dominated by worker speed, not retrier pacing.

**Stale-item detection (`isFailureResolved`)** — catches D-11:

```go
func (e *Engine) isFailureResolved(ctx context.Context, row *SyncFailureRow) bool {
    switch row.Direction {
    case "download":
        // If remote_state is deleted or missing, nothing to download
        rs, _ := e.baseline.GetRemoteState(ctx, row.Path, row.DriveID)
        return rs == nil || rs.SyncStatus == "deleted" || rs.SyncStatus == "synced"
    case "upload":
        // If local file is gone, nothing to upload
        _, err := os.Stat(filepath.Join(e.syncRoot, row.Path))
        return errors.Is(err, os.ErrNotExist)
    case "delete":
        // If baseline entry is gone, nothing to delete
        _, ok := e.baseline.GetByPath(row.Path)
        return !ok
    }
    return false
}
```

These are all local checks (SQLite queries, `os.Stat`). No API calls. Microseconds per item.

**Event creation from DB state (`createEventFromDB`)** — replaces `synthesizeFailureEvent`:

```go
func (e *Engine) createEventFromDB(ctx context.Context, row *SyncFailureRow) *ChangeEvent {
    switch row.Direction {
    case "upload":
        // Upload: observe local file (stat + hash). Free — no API call.
        // The planner needs current hash to avoid re-uploading unchanged files.
        return e.observeLocal(ctx, row.Path)

    default: // download, delete
        // Download/delete: read remote_state from DB.
        // Delta polls keep remote_state fresh during normal operation.
        // No API call — remote_state has hash, size, mtime, etag, item_type.
        rs, _ := e.baseline.GetRemoteState(ctx, row.Path, row.DriveID)
        if rs == nil {
            return nil
        }
        return remoteStateToChangeEvent(rs, row.Path)
    }
}
```

For **uploads**: `observeLocal` calls `os.Stat` + `ComputeQuickXorHash` (free, no API). The planner gets the current local hash. If the hash matches the baseline, the planner skips the upload (no unnecessary re-upload of unchanged files). If the file was modified during the hold, the planner creates an upload with the new hash. If the file was deleted, `observeLocal` returns a ChangeDelete → planner handles appropriately.

For **downloads/deletes**: reads remote_state from the database. Delta polls keep remote_state fresh (CommitObservation updates remote_state for all items in the delta response — store_observation.go:41-81). The resulting ChangeEvent has hash, size, mtime, etag, item_type, path, parent_id, drive_id, item_id. `name` is derived from path. `ctag` is not persisted but is not used by the planner for action classification.

**Fixes D-9** (replaces sparse `synthesizeFailureEvent` with DB-state events for downloads and real observation for uploads), **D-11** (`isFailureResolved` catches orphaned sync_failures).

### 3.9 Trials — PickTrialCandidate + reobserve

Trials source candidates from sync_failures (persistent, queryable) instead of an in-memory held queue. Trials are the ONE place where `reobserve` (with real API calls) is used, because the trial IS the test — we need to actually hit the API to know if the scope has recovered.

**New store method:**

```go
// PickTrialCandidate returns one transient sync_failure matching the scope,
// oldest first. Returns nil if none exist.
func (m *SyncStore) PickTrialCandidate(ctx context.Context,
    scopeKey ScopeKey) (*SyncFailureRow, error)
```

**Trial flow — three stages:**

**Stage 1: Trial timer fires.** Pop candidate from sync_failures, re-observe, mark for interception.

```go
case <-trialTimerCh:
    // Clean stale trial entries (TTL = 2× debounce window)
    e.cleanStaleTrialPending(now)
    if len(e.trialPending) > 0 {
        break // trial already in pipeline, wait for it
    }

    now := e.nowFunc()
    key, _, ok := e.scopeGate.NextDueTrial(now)
    if !ok {
        break
    }

    row, _ := e.baseline.PickTrialCandidate(ctx, key)
    if row == nil {
        // No candidates — scope has no retriable items. Clear block.
        e.onScopeClear(ctx, key)
        break
    }

    ev := e.reobserve(ctx, row.Path, row.ItemID, row.DriveID,
        actionTypeFromDirection(row.Direction))
    if ev == nil {
        // Re-observation failed (429/507 for global scopes) — still blocked.
        e.scopeGate.ExtendTrialInterval(key, ...)
        e.armTrialTimer()
        break
    }

    e.trialPending[row.Path] = trialEntry{scopeKey: key, created: now}
    e.buf.Add(ev) // → planner → fresh action
    e.armTrialTimer()
```

**Stage 2: Fresh action arrives at `routeReadyActions`.** Intercept, validate, mark as trial.

**Goroutine separation**: `routeReadyActions` accesses the `trialPending` map. It is called from the drain goroutine (`processWorkerResult` → `Complete` → dependents). The main goroutine (`processBatch` → `Add` → ready action) must NOT call `routeReadyActions` — it would create a data race on `trialPending`. The main goroutine calls `admitAndDispatch` instead, which does scope admission without trial interception. `trialPending` is drain-goroutine-only state.

```go
// admitAndDispatch — called by main goroutine (processBatch, executePlan).
// No trial interception. No trialPending access. Safe for main goroutine.
func (e *Engine) admitAndDispatch(ctx context.Context, ready []*TrackedAction) {
    anyHeld := false
    for _, ta := range ready {
        if key := e.scopeGate.Admit(ta); !key.IsZero() {
            e.cascadeRecordAndComplete(ctx, ta, key)
            anyHeld = true
        } else {
            e.readyCh <- ta
        }
    }
    if anyHeld {
        e.armTrialTimer()
    }
}

// routeReadyActions — called by drain goroutine (processWorkerResult) ONLY.
// Includes trial interception via trialPending map. NOT safe for main goroutine.
func (e *Engine) routeReadyActions(ctx context.Context, ready []*TrackedAction) {
    anyHeld := false
    for _, ta := range ready {
        if entry, isTrial := e.trialPending[ta.Action.Path]; isTrial {
            delete(e.trialPending, ta.Action.Path)
            // Verify the re-planned action tests the blocked scope
            if entry.scopeKey.BlocksAction(ta.Action.Path,
                ta.Action.ShortcutKey(), ta.Action.Type,
                ta.Action.TargetsOwnDrive()) {
                ta.IsTrial = true
                ta.TrialScopeKey = entry.scopeKey
                e.readyCh <- ta // bypass scope gate
            } else {
                // Re-planned as different type — doesn't test scope.
                // Clear stale sync_failure, re-arm to try next candidate.
                e.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID)
                // Admit normally (won't be blocked — wrong type for scope)
                if key := e.scopeGate.Admit(ta); key.IsZero() {
                    e.readyCh <- ta
                }
                e.armTrialTimer()
            }
            continue
        }

        // Normal admission (same as admitAndDispatch)
        if key := e.scopeGate.Admit(ta); !key.IsZero() {
            e.cascadeRecordAndComplete(ctx, ta, key)
            anyHeld = true
        } else {
            e.readyCh <- ta
        }
    }
    if anyHeld {
        e.armTrialTimer()
    }
}
```

**Stage 3: Worker executes trial.** Unchanged from current design:
- Success → `onScopeClear(key)` → `SetScopeRetryAtNow` → retrier picks up remaining items
- Failure with scope error → `ExtendTrialInterval` → re-arm
- Failure with non-scope error → clear stale sync_failure, re-arm to try next candidate

**Edge cases:**

- **Re-observation returns 404**: Item gone. Event enters buffer as ChangeDelete. Planner produces cleanup/nothing. `BlocksAction` check fails (cleanup doesn't test quota). Stale sync_failure is cleared. Next trial candidate is tried.
- **Re-observation gets 429 (global scope)**: Scope still blocked. `reobserve` returns nil. Trial interval extended. No event injected.
- **Re-observation gets 200 (quota scope)**: GET doesn't test quota. Event injected. Planner produces upload. Upload IS the trial — worker executes it. If 507 → scope still blocked. If success → scope clear.
- **Planner produces no action** (item already synced): `trialPending` entry lingers. TTL cleanup (30 seconds) removes it. Next trial timer picks another candidate.
- **Watch event arrives for same path**: Buffer merges events for same path (PathChanges). Latest event wins. `trialPending` match still fires since path matches.

### 3.10 Backoff Timing

Unified constants replace scope-specific ones:

```go
defaultInitialTrialInterval = 5 * time.Second  // without Retry-After
defaultMaxTrialInterval     = 5 * time.Minute  // cap when no Retry-After
```

**With Retry-After**: Use the header value directly as the trial interval. No cap — server is ground truth.

**Without Retry-After**: Exponential backoff: 5s → 10s → 20s → 40s → 80s → 160s → 300s (capped at 5 minutes).

`ExtendTrialInterval` signature changes to accept `retryAfter time.Duration`. If `retryAfter > 0`, use directly. If zero, double current interval up to max.

### 3.11 Shortcut Enrichment (D-5)

`remoteStateFromEvent()` must propagate `RemoteDriveID` and `RemoteItemID`:

1. Add `RemoteDriveID string` and `RemoteItemID string` to `RemoteState`
2. Populate in `remoteStateFromEvent()` from `ev.RemoteDriveID` / `ev.RemoteItemID`
3. In `makeAction()`, if `view.Remote.RemoteDriveID != ""`, set `action.targetShortcutKey` and `action.targetDriveID`

The rest of the pipeline (`ShortcutKey()`, `TargetDriveID()`, `WorkerResult`, `classifyResult`, `BlocksAction`) is already wired — it just receives empty values today. Sequencing: do AFTER the split so scope blocking is architecturally correct before activating it.

### 3.12 sync_failures Ownership (D-6)

Remove `DELETE FROM sync_failures` from `updateRemoteStateOnOutcome()` (store_baseline.go:472-536). The engine owns failure lifecycle via `clearFailureOnSuccess` (renamed from `defensiveClearFailure`). The store owns baseline state transitions.

---

## 4. Migration Plan

**Phase ordering note**: Phase 1 modifies `scope.go` and `engine.go`. Phases 2-4 also heavily modify `engine.go`. If Phase 1 is merged to main before Phases 2-4 start, Phases 2-4 will have merge conflicts in `engine.go`. Recommendation: do Phase 1 in the same worktree/branch as Phases 2-4 to avoid conflicts, or accept that Phase 4's engine rewrite will need to incorporate Phase 1's constant changes.

### Phase 1: Backoff Timing

Independent. Can be done first (see ordering note above).

1. Replace scope-specific constants with `defaultInitialTrialInterval` (5s) and `defaultMaxTrialInterval` (5min)
2. Simplify `MaxTrialInterval()` — all scope types return `defaultMaxTrialInterval`
3. Change `extendTrialInterval` to accept `retryAfter time.Duration`
4. Change `UpdateScope()` initial intervals to use `defaultInitialTrialInterval` or `retryAfter`
5. Update requirements: R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.14
6. Update design docs: sync-execution.md, sync-engine.md

### Phase 2: Extract DepGraph

1. Create `dep_graph.go` with `DepGraph` type
2. Move from `tracker.go`: `Add` (modified to return `*TrackedAction`), `Complete` (modified to return `[]*TrackedAction`), `HasInFlight`, `CancelByPath`, `InFlightCount`, `Done`
3. Fix: `Complete` must `delete(dt.actions, id)` (currently missing — D-10)
4. Move `TrackedAction` struct
5. Unit tests in `dep_graph_test.go`

### Phase 3: Extract ScopeGate + Persist Scope Blocks

1. Create `scope_gate.go` with `ScopeGate` type (no held queue)
2. Create `scope_blocks` table (migration in `migrations.go`)
3. Move from `tracker.go`: `HoldScope` → `SetScopeBlock` (persists), `blockedScope` → `Admit`, scope block methods
4. Remove: `ReleaseScope`, `DiscardScope`, `DispatchTrial`, `PopTrial` (all depended on held queue)
5. Remove: `held map[ScopeKey][]*TrackedAction`, `onHeld func()`
6. Add: `IsScopeBlocked`, `LoadFromStore`
7. `NextDueTrial` / `EarliestTrialAt` — remove held-queue-length checks
8. Unit tests in `scope_gate_test.go`

### Phase 4: Rewire Engine

1. Replace `tracker *DepTracker` with `depGraph *DepGraph`, `scopeGate *ScopeGate`, `readyCh chan *TrackedAction`
2. Promote `buf` from local variable to engine field (currently local in `initWatchPipeline`, engine.go:1012; retrier in drain loop needs `e.buf.Add`)
3. Create `reobserve`, `observeLocal`, `observeRemote` (trial use only)
4. Create `createEventFromDB`, `remoteStateToChangeEvent` (retrier use)
5. Create `isFailureResolved` (stale sync_failure detection — D-11)
6. Create `admitAndDispatch` (main goroutine — no trial interception) and `routeReadyActions` (drain goroutine — with trial interception via `trialPending` map). `trialPending` is drain-goroutine-only state — no cross-goroutine access.
7. Create `cascadeRecordAndComplete`, `recordScopeBlockedFailure`, `resetDispatchStatus`
8. Create `onScopeClear` — `ClearScopeBlock` + `SetScopeRetryAtNow` + `armRetryTimer` (no inline re-observation)
9. Rewrite `processWorkerResult` with failure-aware dependent dispatch (section 3.5)
10. Rewrite `processTrialResult` — success calls `onScopeClear`, failure extends interval
11. Integrate retrier into drain loop: retry timer in select, batch-limited (zero-delay re-arm when batch full), `isFailureResolved` check, `createEventFromDB`
12. Rewrite trial timer: `PickTrialCandidate` + `reobserve` + `trialPending`
13. Rewrite `handleRemovedShortcuts`: `ClearScopeBlock` + `DeleteSyncFailuresByScope`
14. Rewrite `handleExternalChanges`: `ClearScopeBlock` → `onScopeClear`
15. Remove `onHeld` callback, `trialCh` channel
16. On startup: `scopeGate.LoadFromStore(ctx)`, arm trial timer
17. Create `ResetDispatchStatus` in `SyncStore`
18. Create `PickTrialCandidate` in `SyncStore`
19. Create `SetScopeRetryAtNow` in `SyncStore` — `UPDATE sync_failures SET next_retry_at = ? WHERE scope_key = ?`
20. Update `WorkerPool` to accept `readyCh <-chan *TrackedAction` and `done <-chan struct{}`
20. Rewrite `executePlan` (one-shot): no scope gate (section 2.3)
21. Rewrite `processBatch` (watch): scope gate checks after Add

### Phase 5: Delete Old Code

1. Delete `tracker.go`, `tracker_test.go`
2. Delete `FailureRetrier` goroutine from `reconciler.go`
3. Delete `synthesizeFailureEvent`
4. Delete `dispatchedRetryAt` dedup map
5. Keep `InFlightChecker` interface (or move to engine)

### Phase 6: Shortcut Enrichment (D-5)

1. Add `RemoteDriveID`, `RemoteItemID` to `RemoteState`
2. Populate in `remoteStateFromEvent()` and `makeAction()`
3. Tests for shortcut scope blocking

### Phase 7: sync_failures Ownership (D-6)

1. Remove `DELETE FROM sync_failures` from `updateRemoteStateOnOutcome()`
2. Rename `defensiveClearFailure` → `clearFailureOnSuccess`
3. Update design docs

---

## 5. Defect-to-Fix Traceability

| Defect | Phase | How fixed |
|---|---|---|
| D-1: Data race in `dispatch()` | 2, 4 | `dispatch()` eliminated. DepGraph returns slices under lock. Channel sends in engine, no lock. |
| D-2: Lock ordering `dt.mu` → `trialMu` | 3, 4 | `onHeld` callback eliminated. `armTrialTimer()` called inline by engine. No cross-lock paths. |
| D-3: `DiscardScope` orphans dependents | 3, 4 | `DiscardScope` eliminated. `Complete` is the single terminal path. `cascadeRecordAndComplete` for scope-blocked subtrees. |
| D-4: Channel-send-under-lock | 2, 4 | `DepGraph.Add` returns ready action (no channel). Engine sends outside lock. |
| D-5: Shortcut enrichment gap | 6 | Planner propagates `RemoteDriveID`/`RemoteItemID` through to Action. |
| D-6: Dual sync_failures clearing | 7 | Engine owns failure lifecycle. Store owns baseline commits. No overlap. |
| D-7: Stale held actions dispatched | 3, 4 | No held queue. Blocked actions → sync_failures + Complete. Scope clear → retrier re-processes from DB state. |
| D-8: Held queue lost on crash | 3 | No held queue. sync_failures are persistent. Scope blocks are persisted in `scope_blocks` table. |
| D-9: Sparse fake events in retrier | 4 | `synthesizeFailureEvent` replaced by `createEventFromDB` (full remote_state for downloads, `observeLocal` for uploads). |
| D-10: `Complete` doesn't delete from `dt.actions` | 2 | `DepGraph.Complete` deletes from `actions` map after copying dependents. Prerequisite for scope-blocked immediate completion. |
| D-11: Orphaned sync_failures | 4 | `isFailureResolved` check in retrier sweep: verifies item still needs action before re-injecting. Clears resolved sync_failures. |

---

## 6. Invariants

1. **Single terminal path.** Every action exits the graph via `DepGraph.Complete()`. No second bookkeeping path.

2. **No channel sends under any mutex.** `DepGraph.mu` protects graph state. `ScopeGate.mu` protects scope state. `readyCh` is written only by the engine, outside both locks.

3. **No cross-lock acquisition.** No code path holds two locks simultaneously.

4. **Policy in the engine, mechanics in the data structures.** DepGraph manages ordering. ScopeGate manages admission. The engine decides what happens when parents fail, scopes clear, or shortcuts are deleted.

5. **Scope-blocked actions are never dispatched to workers.** Blocked → sync_failure + Complete. Scope clear → retrier re-processes from DB state. No stale PathViews reach workers.

6. **Scope blocks are persistent.** `scope_blocks` table survives crash. Trial timer resumes on startup. No crash recovery gap.

7. **Retrier creates events from DB state.** For downloads/deletes: reads remote_state (kept fresh by delta polls). For uploads: `observeLocal` (stat + hash, free). No API calls in the retrier sweep. `reobserve` (real API calls) is used ONLY for trials.

8. **Retrier and scope clear don't overlap.** Retrier handles per-item failures (`next_retry_at`). Scope-blocked items have `next_retry_at = NULL` — invisible to retrier. `onScopeClear` sets `next_retry_at = NOW` to make them visible, then the retrier handles them like any other item.

9. **Failure-aware dependent dispatch.** Parent fails → children cascade-completed and recorded as sync_failures. Parent succeeds → children dispatched. The graph doesn't know about failure; the engine applies policy.

10. **Persistent state is always cleaned up.** `setDispatch` writes are reversed by `resetDispatchStatus` when actions are scope-blocked or cascade-completed.

11. **Stale sync_failures are detected and cleared.** `isFailureResolved` checks DB state (remote_state, baseline, local filesystem) before re-injecting. Items whose underlying condition resolved through the normal pipeline are cleared, not retried forever.

12. **The state machine is authoritative.** Any code change modifying states or transitions MUST update the design specs first.

13. **Trial logic documented in one place.** The trial mechanism (scope blocks, trial timer, PickTrialCandidate, reobserve, trial interception, processTrialResult) is documented in sync-execution.md under a single "Scope Blocks and Trials" section. sync-engine.md references it but does not duplicate it. The previous split (tracker in sync-execution.md, trial timer in sync-engine.md) caused interaction bugs (A1/A2) because neither doc owned the full contract.

---

## 7. Files Changed

| File | Change | Phase |
|---|---|---|
| `internal/sync/scope.go` | Unified backoff constants, simplified `MaxTrialInterval` | 1 |
| `internal/sync/dep_graph.go` | **New** — DepGraph type | 2 |
| `internal/sync/dep_graph_test.go` | **New** | 2 |
| `internal/sync/scope_gate.go` | **New** — ScopeGate type (no held queue, persisted blocks) | 3 |
| `internal/sync/scope_gate_test.go` | **New** | 3 |
| `internal/sync/migrations.go` | Add `scope_blocks` table | 3 |
| `internal/sync/store_failures.go` | Add `PickTrialCandidate`, `SetScopeRetryAtNow` | 4 |
| `internal/sync/store_admin.go` | Add `ResetDispatchStatus` | 4 |
| `internal/sync/engine.go` | Replace tracker with DepGraph + ScopeGate + readyCh. Promote `buf` to engine field. Add `reobserve` (trials only), `createEventFromDB`, `isFailureResolved`, `admitAndDispatch` (main goroutine), `routeReadyActions` (drain goroutine, trial interception), `cascadeRecordAndComplete`, `onScopeClear`. Failure-aware dispatch. Retrier in drain loop (batch-limited, zero-delay re-arm). Startup loads scope blocks. | 4 |
| `internal/sync/engine_test.go` | Rewrite for new types | 4 |
| `internal/sync/engine_shortcuts.go` | `ClearScopeBlock` + `DeleteSyncFailuresByScope` | 4 |
| `internal/sync/worker.go` | Accept `readyCh` and `done` channels | 4 |
| `internal/sync/worker_test.go` | Update construction | 4 |
| `internal/sync/tracker.go` | **Deleted** | 5 |
| `internal/sync/tracker_test.go` | **Deleted** | 5 |
| `internal/sync/reconciler.go` | Delete `FailureRetrier`, `synthesizeFailureEvent`, `dispatchedRetryAt`. Keep `InFlightChecker` interface. | 5 |
| `internal/sync/reconciler_test.go` | Rewrite for engine-integrated retry | 5 |
| `internal/sync/types.go` | Add `RemoteDriveID`, `RemoteItemID` to `RemoteState` | 6 |
| `internal/sync/planner.go` | Populate shortcut fields | 6 |
| `internal/sync/store_baseline.go` | Remove sync_failures DELETE from `updateRemoteStateOnOutcome` | 7 |
| `spec/design/sync-execution.md` | DepGraph + ScopeGate. State machine. Persisted scope blocks. Trial logic (single location). | 4 |
| `spec/design/sync-engine.md` | Scope clear, retrier, failure-aware dispatch, backoff. References sync-execution.md for trial logic. | 1, 4 |
| `spec/requirements/sync.md` | R-2.10.5-8, R-2.10.11, R-2.10.14 updates | 1, 4 |

---

## 8. Risks and Mitigations

**Risk**: Phase 4 (engine rewiring) is a large change touching the core orchestration loop.
**Mitigation**: Phases 1-3 are additive (new files, new tests, backoff constants). Phase 4 is the swap. Existing test suite provides regression coverage. Run with `-race`.

**Risk**: `PickTrialCandidate` returns a stale candidate (file deleted since failure was recorded).
**Mitigation**: `reobserve` returns 404 → planner produces no matching action → `trialPending` entry times out → next candidate is tried. If all candidates are stale, `PickTrialCandidate` eventually returns nil → scope block cleared.

**Risk**: Trial `trialPending` entry lingers (planner produces no action for the path).
**Mitigation**: TTL-based cleanup (30 seconds = 2× debounce window). Trials serialized — only one pending at a time per engine.

**Risk**: Uncapped Retry-After header (malformed → decades-long block).
**Accepted**: User explicitly decided to trust the server. No cap applied.

**Risk**: Scope-blocked sync_failures accumulate if scope block is never cleared (API permanently broken).
**Mitigation**: Scope blocks have trial intervals that cap at 5 minutes. Trials continue indefinitely. If the API never recovers, the items sit in sync_failures and are visible to the user via `onedrive issues`. This is correct behavior — the system reports the problem and keeps trying.

**Risk**: `routeReadyActions` blocking on `readyCh` could block the drain loop.
**Mitigation**: Same as today — buffer sizing (1024 in watch mode). The drain loop already processes results sequentially. If needed, actor-with-outbox pattern is a future optimization.

**Risk**: Retrier batch size too small for scope clears (100k items at 16/sweep = slow recovery).
**Mitigation**: `retryBatchSize` is configurable. Default `2 × transferWorkers`. After scope clear, workers are the bottleneck — they execute at `transferWorkers` parallelism regardless of how fast the retrier feeds the pipeline. The retrier just needs to keep the readyCh non-empty. A batch size of `1024` (readyCh buffer size) keeps workers saturated without stalling the drain loop.

**Risk**: `isFailureResolved` adds DB queries per item in retrier sweep.
**Mitigation**: Local SQLite queries — microseconds per item. Bounded by `retryBatchSize`. The check prevents infinite retries of stale sync_failures, which would waste more resources than the check itself.
