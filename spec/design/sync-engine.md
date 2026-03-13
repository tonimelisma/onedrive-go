# Sync Engine

GOVERNS: internal/sync/engine.go, internal/sync/engine_shortcuts.go, internal/sync/delete_counter.go, internal/sync/orchestrator.go, internal/sync/drive_runner.go, sync.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.6 [verified], R-2.8 [verified], R-3.4.2 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.3 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.9 [verified], R-2.10.10 [planned], R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.14 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.24 [verified], R-2.10.25 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified], R-2.10.30 [verified], R-2.10.31 [planned], R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified], R-2.10.38 [verified], R-6.4.1 [verified], R-6.4.2 [verified], R-6.4.3 [verified], R-6.6.7 [verified], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned], R-6.7.27 [verified], R-6.8.15 [verified]

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Runs `RunOnce` in a loop, triggered by filesystem events and delta polling.

Watch mode uses a unified tick loop: filesystem events are debounced by the change buffer, remote changes are polled at `poll_interval` (default 5 minutes). Periodic full reconciliation runs every 24 hours to detect missed delta deletions.

### Error Classification (`classifyResult()`)

Implements: R-6.8.9 [verified], R-6.8.15 [verified], R-6.7.27 [verified]

Pure function `classifyResult(*WorkerResult) → (resultClass, scopeKey)`. Single classification point for all worker results. No side effects — classification is separate from routing ("functions do one thing"). Six result classes:

- `resultSuccess`: action succeeded
- `resultRequeue`: transient failure — re-queue with backoff
- `resultScopeBlock`: scope-level failure (429, 507, 5xx pattern) — feed scope detection
- `resultSkip`: non-retryable — record and move on
- `resultShutdown`: context canceled — discard silently, no failure recorded
- `resultFatal`: abort sync pass (401 unrecoverable auth)

Classification table: 401 → fatal, 403 → skip (with handle403 side effect), 429 → scopeBlock `throttle:account`, 507 → scopeBlock `quota:own` or `quota:shortcut:$key`, 400 + outage pattern → requeue, 5xx → requeue, 408/412/404/423 → requeue, context.Canceled → shutdown, os.ErrPermission → skip.

`isOutagePattern()` detects known 400 outage patterns (e.g., "ObjectHandle is Invalid") that are actually transient service outages. Distinguished from phantom drive 400s (R-6.7.11) by error body inspection.

### Scope Detection and Management

Implements: R-2.10.3 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [planned], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified]

`processWorkerResult()` classifies each result and routes it — all cases call `tracker.Complete()` (never `ReQueue`):

- **success** → `Complete` + `RecordSuccess` (scope window reset) + counter
- **requeue** (transient) → `recordFailure` with `retry.Reconcile.Delay` + `Complete` + `feedScopeDetection` + `retrier.Kick()`
- **scopeBlock** (429, 507) → `recordFailure` with `retry.Reconcile.Delay` + `feedScopeDetection` + `Complete` + `armTrialTimer()` (belt-and-suspenders) + `retrier.Kick()`
- **skip** (non-retryable) → handle403 side effect + `recordFailure` with nil delayFn (no `next_retry_at`) + `Complete`
- **shutdown** → `Complete` (no failure recorded)
- **fatal** (401) → `recordFailure` with nil delayFn + `Complete`

Trial result routing: `handleTrialResult()` runs before classification. Trial success → `tracker.ReleaseScope(scopeKey)` + `resetScopeRetryTimes(scopeKey, now)` (thundering herd: resets `next_retry_at` for all sync_failures matching the scope, then kicks the retrier) + `armTrialTimer()`. Trial failure → reads block's current `TrialInterval` via `tracker.GetScopeBlock(scopeKey)` (value copy), doubles it, caps per scope type (`maxTrialIntervalForIssueType`), calls `tracker.ExtendTrialInterval(scopeKey, nextAt, newInterval)` (encapsulates mutation under lock) + `armTrialTimer()`. Per-scope caps: quota 1h, rate_limited 10m, service 10m (R-2.10.6/R-2.10.8/R-2.10.14).

**Trial timer**: `armTrialTimer()` uses `time.AfterFunc` to send to a persistent `trialCh` channel when the earliest `NextTrialAt` across all scope blocks is reached (via `tracker.EarliestTrialAt()`). Using a persistent channel avoids a race where `onHeld` (called from external goroutines via `tracker.Add`) replaces the timer while the drain loop's select watches the old timer's channel. The `trialCh` fires in `drainWorkerResults`'s select loop, calling `tracker.NextDueTrial(now)` + `tracker.DispatchTrial(key)` in a loop until no more trials are due. Called after `applyScopeBlock()`, `handleTrialResult()`, trial dispatch, and via the `onHeld` callback when actions enter held queues. Belt-and-suspenders: `armTrialTimer()` is also called after `tracker.Complete` in the `resultScopeBlock` case to catch dependents that entered held.

`feedScopeDetection()` feeds results into `ScopeState.UpdateScope()`. When a threshold is crossed, creates a scope block via `applyScopeBlock()` which calls `tracker.HoldScope()` + `armTrialTimer()`.

The engine owns all completion decisions — workers are pure executors. Engine-owned counters (`succeeded`, `failed` atomics) replace the worker-owned counters removed in this refactoring. `drainWorkerResults()` processes results concurrently for both one-shot and watch modes.

`recordFailure()` sets category based on `delayFn`: non-nil → `"transient"`, nil → `"actionable"`. Populates `ScopeKey` via `deriveScopeKey(r)` from HTTP status and shortcut context. Delegates to `SyncStore.RecordFailure()` which computes `next_retry_at` via the `delayFn`. The `FailureRetrier` sweeps `sync_failures` for due items and re-injects them via buffer → planner → tracker.

### ScopeState

Implements: R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified]

In-memory data structure in `scope.go`: sliding windows (scope_key → `slidingWindow`) for scope escalation detection. Engine-internal — no cross-engine coordination (each engine discovers independently). Scope blocks are stored in the tracker's `scopeBlocks` map and enforce held queuing.

### Scanner ScanResult Contract

Implements: R-2.11.5 [implemented], R-2.10.2 [planned]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via two methods:

- **`recordSkippedItems(skipped []SkippedItem)`** — Groups skipped items by reason, batch-upserts to `sync_failures` as actionable failures. Uses aggregated logging: when >10 items share the same reason, logs 1 WARN summary with count and sample paths, individual paths at DEBUG. When <=10 items, logs each as an individual WARN.
- **`clearResolvedSkippedItems(skipped []SkippedItem)`** — Deletes `sync_failures` entries for files that are no longer skipped (e.g., user renamed a previously invalid file). Compares current skipped paths against recorded failures and removes stale entries.

### Aggregated Logging

Implements: R-6.6.7 [verified], R-6.6.8 [planned], R-6.6.9 [planned], R-6.6.10 [planned], R-6.6.12 [planned]

When >10 items share the same warning category, log 1 WARN summary with count and sample paths + individual paths at DEBUG. When <=10 items, log each as an individual WARN. This pattern is implemented in `recordSkippedItems()` for scanner-time validation failures. Transient retries at DEBUG, resolved at INFO, exhausted at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

### Planned: Local Permission Handling

Implements: R-2.10.12 [planned], R-2.10.13 [planned]

`os.ErrPermission` → check parent directory accessibility. Inaccessible directory: one `local_permission_denied` at directory level, suppress operations under it. Accessible directory: file-level failure. Recheck directory-level issues at start of each sync pass.

### Planned: Observation Suppression

Implements: R-2.10.30 [verified], R-2.10.31 [planned]

During `throttle:account` or `service` scope block, suppress shortcut observation polling (wastes API calls). During `quota:shortcut:*` block, observation continues (read-only).

### Shortcut Integration (`engine_shortcuts.go`)

Detects shortcuts to shared folders in the delta stream. Creates additional delta scopes for shared folder observation. Handles shortcut removal (cleanup local copies).

## Orchestrator (`orchestrator.go`)

Multi-drive coordination. Runs one `DriveRunner` per configured, non-paused drive. Each drive gets its own goroutine, state DB, and sync engine instance. Engines do not coordinate scope blocks across engine boundaries — each engine discovers independently. Bounded waste accepted (one request per engine for 429). Implements: R-2.10.35 [planned]

Handles:
- Drive add/remove via SIGHUP config reload
- Pause/resume per drive
- Graceful shutdown (drain all drives)

## DriveRunner (`drive_runner.go`)

Per-drive lifecycle manager. Creates the engine, opens the state DB, and runs the sync loop. Handles drive-level errors and restart.

## CLI Sync Command (`sync.go`, `sync_helpers.go`)

Cobra command wiring. Sets up the orchestrator, handles `--watch`, `--download-only`, `--upload-only`, `--dry-run`, `--full`, `--drive` flags. Signal handling: first SIGINT = drain, second = force exit.

## Watch Mode Behavior

- SIGHUP → reload `config.toml`, apply drive changes immediately
- PID file with flock for single-instance enforcement
- Two-signal shutdown (drain, then force)
- Periodic full reconciliation (default 24h)

### Watch-Mode Big-Delete Protection (`delete_counter.go`)

Implements: R-6.4.2 [verified], R-6.4.3 [verified]

In watch mode, the planner-level big-delete check is disabled (`threshold=MaxInt32`) because 2-second debounced batches would fragment a mass delete across many small batches, each below threshold. Instead, a rolling-window `deleteCounter` accumulates planned deletes across batches.

**Counter**: `deleteCounter` tracks timestamps of planned delete actions within a configurable rolling window (5 minutes). When the cumulative count exceeds `big_delete_threshold`, the counter latches `held=true`. Expired entries (older than the window) are pruned on each `Add()` call.

**Flow in `processBatch()`**: After `planner.Plan()` returns, the engine counts `ActionLocalDelete` + `ActionRemoteDelete` actions and calls `counter.Add(count)`. If `counter.IsHeld()`:
1. Delete actions are filtered out of the plan (via `applyDeleteCounter()`)
2. Non-delete actions continue to the tracker and execute normally
3. Held deletes are recorded as `sync_failures` rows with `issue_type=big_delete_held` via `UpsertActionableFailures()`

**CLI notification**: `issues list` shows held deletes in a dedicated "HELD DELETES" section. User approves via `issues clear --all` (or `issues clear <path>` for individual files).

**External change detection**: A 10-second `recheckTicker` in the `RunWatch()` select loop runs `PRAGMA data_version` to detect CLI writes. When the data version changes, `handleExternalChanges()` queries `ListSyncFailuresByIssueType(IssueBigDeleteHeld)`. If zero rows remain (user cleared them all), calls `counter.Release()`. On the next observation cycle, deletions are re-observed and dispatched normally.

**Startup cleanup**: `RunWatch()` clears stale `big_delete_held` entries from prior daemon sessions, since the in-memory counter resets on restart.

**Force mode**: `--force` skips counter creation (`deleteCounter` stays nil), so no watch-mode big-delete protection applies.

### Rationale

- **Crash recovery requires explicit bridging**: On restart after crash, `ResetInProgressStates` resets `remote_state` items stuck mid-execution to pending, AND creates `sync_failures` entries so the `FailureRetrier`'s bootstrap sweep can rediscover them. This is necessary because the delta token was already advanced before execution — items that crashed mid-execution won't appear in the next delta response. The planner is idempotent for items that DO appear in observations, but crash recovery items need the `sync_failures` → `FailureRetrier` → buffer → planner path.
- **Always use Orchestrator, even for single drive**: N=1 means one DriveRunner — same logic, no special case. Prevents "works for N=1 but breaks for N=2" class of bugs.
