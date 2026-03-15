# Sync Engine

GOVERNS: internal/sync/engine.go, internal/sync/engine_shortcuts.go, internal/sync/delete_counter.go, internal/sync/orchestrator.go, internal/sync/drive_runner.go, sync.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.6 [verified], R-2.8 [verified], R-3.4.2 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.3 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.9 [verified], R-2.10.10 [verified], R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.14 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.24 [verified], R-2.10.25 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified], R-2.10.30 [verified], R-2.10.31 [verified], R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified], R-2.10.38 [verified], R-2.10.43 [verified], R-6.4.1 [verified], R-6.4.2 [verified], R-6.4.3 [verified], R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [planned], R-6.6.10 [verified], R-6.6.12 [verified], R-6.7.27 [verified], R-6.8.15 [verified]

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Flow: `initWatchInfra → bootstrapSync → startObservers → runWatchLoop`.

Watch mode uses a unified tick loop: filesystem events are debounced by the change buffer, remote changes are polled at `poll_interval` (default 5 minutes). Periodic full reconciliation runs every 24 hours to detect missed delta deletions.

### Unified Bootstrap (Engine Pipeline Redesign Phase 9)

`RunWatch` no longer calls `RunOnce` for its initial sync. Instead:

1. **`initWatchInfra`** creates watchState, DepGraph, ScopeGate, WorkerPool, Buffer, and tickers — but does NOT load baseline, start observers, or launch the drain goroutine.
2. **`bootstrapSync`** loads baseline, starts the drain goroutine, observes changes, and dispatches them through the same DepGraph/ScopeGate/WorkerPool that the watch loop uses. Blocks until all bootstrap actions complete via `waitForQuiescence`.
3. **`startObservers`** launches remote and local observers AFTER bootstrap — they see the post-bootstrap baseline.
4. **`runWatchLoop`** runs the steady-state select loop.

**`waitForQuiescence`** blocks until `DepGraph.WaitForEmpty()` fires, with a 30-minute safety timeout and periodic progress logging. `WaitForEmpty` is a one-shot channel: closed when `len(actions) == 0` after `Complete()` deletes an entry.

**Why not RunOnce?** The old approach created throwaway infrastructure (DepGraph, workers, readyCh) in `RunOnce`, then `initWatchPipeline` created a completely new set. Unified bootstrap creates infrastructure once and reuses it for both the initial sync and steady-state.

`RunOnce` remains unchanged as a standalone one-shot entry point.

### watchState (Engine Pipeline Redesign Phase 8)

`watchState` bundles all watch-mode-only Engine fields. `e.watch` is nil in
one-shot mode (`RunOnce`), non-nil in watch mode (`RunWatch`). Methods use
`e.watch != nil` as a single guard replacing individual field nil-checks.

Fields on watchState: scopeGate, scopeState, buf, deleteCounter,
lastDataVersion, trialPending, trialTimer, trialMu, retryTimer,
retryTimerCh, remoteObs, localObs, nextActionID, lastPermRecheck,
lastSummaryTotal.

Fields remaining on Engine (used in both modes): depGraph, readyCh,
trialCh, watchShortcuts, watchShortcutsMu. `watchShortcuts` stays on Engine
because `setWatchShortcuts` is called from `RunOnce` where `e.watch == nil`;
moving it would break 403 handling in one-shot mode.

`deleteCounter` requires a double guard (`e.watch != nil &&
e.watch.deleteCounter != nil`) because watch mode with `force=true` skips
delete counter creation. `retryTimer` requires a specific nil check
(`e.watch.retryTimer != nil`) because it is lazily created via
`time.AfterFunc`.

### Error Classification (`classifyResult()`)

Implements: R-6.8.9 [verified], R-6.8.15 [verified], R-6.7.27 [verified]

Pure function `classifyResult(*WorkerResult) → (resultClass, ScopeKey)`. Single classification point for all worker results. Returns a typed `ScopeKey` (not a string) for scope-block results; zero-value `ScopeKey` for non-scope classes. No side effects — classification is separate from routing ("functions do one thing"). Six result classes:

- `resultSuccess`: action succeeded
- `resultRequeue`: transient failure — re-queue with backoff
- `resultScopeBlock`: scope-level failure (429, 507, 5xx pattern) — feed scope detection
- `resultSkip`: non-retryable — record and move on
- `resultShutdown`: context canceled — discard silently, no failure recorded
- `resultFatal`: abort sync pass (401 unrecoverable auth)

Classification uses `ScopeKeyForStatus(httpStatus, shortcutKey)` as the single source of truth for HTTP status → scope key mapping: 401 → fatal, 403 → skip (with handle403 side effect), 429 → scopeBlock `SKThrottleAccount`, 507 → scopeBlock `SKQuotaOwn` or `SKQuotaShortcut(key)`, 400 + outage pattern → requeue, 5xx → requeue, 408/412/404/423 → requeue, context.Canceled → shutdown, os.ErrPermission → skip.

`isOutagePattern()` detects known 400 outage patterns (e.g., "ObjectHandle is Invalid") that are actually transient service outages. Distinguished from phantom drive 400s (R-6.7.11) by error body inspection.

### Scope Detection and Management

Implements: R-2.10.3 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified]

`processWorkerResult()` classifies each result and routes it — all cases call `depGraph.Complete()`:

- **success** → `Complete` + `RecordSuccess` (scope window reset) + counter + `clearFailureOnSuccess` (engine owns failure lifecycle exclusively — D-6)
- **requeue** (transient) → `recordFailure` with `retry.Reconcile.Delay` + `Complete` + `feedScopeDetection` + arm retry timer
- **scopeBlock** (429, 507) → `recordFailure` with `retry.Reconcile.Delay` + `feedScopeDetection` + `Complete` + `armTrialTimer()` (belt-and-suspenders)
- **skip** (non-retryable) → handle403 side effect + `recordFailure` with nil delayFn (no `next_retry_at`) + `Complete`
- **shutdown** → `Complete` (no failure recorded)
- **fatal** (401) → `recordFailure` with nil delayFn + `Complete`

Scope-blocked actions are not held in memory. Instead, `processWorkerResult` records the failure in `sync_failures` and calls `depGraph.Complete()`. When the scope clears, `onScopeClear` resets `next_retry_at` for matching `sync_failures` rows, and the drain-loop retrier re-injects them via buffer, planner, and DepGraph.

Trial result routing: `processTrialResult()` handles all trial outcomes with an early return — trial results never enter the normal `processWorkerResult()` switch. The `TrialScopeKey ScopeKey` from `WorkerResult` identifies the scope. Trial success → `onScopeClear(scopeKey)` (`ClearScopeBlock` + `SetScopeRetryAtNow` + arm retry timer) + `armTrialTimer()`. Trial failure → `extendTrialInterval(scopeKey)` reads block's current `TrialInterval` via `scopeGate.GetScopeBlock(scopeKey)` (value copy), doubles it, caps per scope type via `computeTrialInterval()`, calls `scopeGate.ExtendTrialInterval(scopeKey, nextAt, newInterval)`. Scope detection is intentionally NOT called for trial failures — the scope is already blocked, and re-detecting would overwrite the doubled interval. Per-scope caps: quota 1h, rate_limited 10m, service 10m (R-2.10.6/R-2.10.8/R-2.10.14).

**Trial dispatch**: `runTrialDispatch()` is called from the drain loop's select when the trial timer fires. Uses `scopeGate.AllDueTrials(now)` to snapshot all due scope blocks at once, then iterates each exactly once — structurally incapable of infinite iteration. For each scope, uses `PickTrialCandidate` from `sync_failures` to find an actual item for re-observation and dispatch. `reobserve` returns `(*ChangeEvent, time.Duration)` — the `RetryAfter` duration is forwarded to `extendTrialInterval` when the scope condition persists (R-2.10.7). On successful dispatch, the trial interval is NOT extended (awaits worker result). Trial actions are marked `IsTrial=true` with `TrialScopeKey` set. `armTrialTimer()` uses `time.AfterFunc` to send to a persistent `trialCh` channel when the earliest `NextTrialAt` across all scope blocks is reached (via `scopeGate.EarliestTrialAt()`). Called after `applyScopeBlock()`, `processTrialResult()`, and trial dispatch.

**Drain-loop retrier**: Retry is integrated directly into the drain loop via `runRetrierSweep()` — no separate goroutine. The drain loop's select includes a retry timer that triggers sweeps of `sync_failures` for items whose `next_retry_at` has expired. Each sweep is batch-limited with zero-delay re-arm when the batch is full. Items are checked via `isFailureResolved()` before re-injection (D-11 fix: prevents re-dispatching items whose underlying condition has resolved). Re-injection uses `createEventFromDB` (full `remote_state` for downloads, `observeLocal` for uploads) to feed items through the normal buffer, planner, and DepGraph pipeline.

`feedScopeDetection()` feeds results into `ScopeState.UpdateScope()`. When a threshold is crossed, creates a scope block via `applyScopeBlock()` which calls `scopeGate.SetScopeBlock()` + `armTrialTimer()`.

The engine owns all completion decisions — workers are pure executors. The drain loop uses an actor-with-outbox pattern: results are processed single-threaded within the drain goroutine, and ready actions are collected into an outbox slice before being sent to the `readyCh` channel. This prevents deadlock that would occur if the drain loop tried to send to a full `readyCh` while workers tried to send to a full `resultCh`. Engine-owned counters (`succeeded`, `failed` atomics) track progress. `drainWorkerResults()` processes results for both one-shot and watch modes.

`recordFailure()` sets category based on `delayFn`: non-nil → `"transient"`, nil → `"actionable"`. Populates `scope_key` via `ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)` — returns a typed `ScopeKey`, serialized to wire format via `String()` for SQLite storage. Delegates to `SyncStore.RecordFailure()` which computes `next_retry_at` via the `delayFn`. The drain-loop retrier sweeps `sync_failures` for due items and re-injects them via buffer → planner → DepGraph.

### ScopeState

Implements: R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified]

In-memory data structure in `scope.go`: sliding windows (`ScopeKey` → `slidingWindow`) for scope escalation detection. All keys are typed `ScopeKey` structs (see sync-execution.md § ScopeKey Type System). Engine-internal — no cross-engine coordination (each engine discovers independently). Scope blocks are managed by `ScopeGate` with write-through persistence to the `scope_blocks` table.

### `disk:local` Scope Block

Implements: R-2.10.43 [verified]

Scope key `SKDiskLocal` is created by `classifyResult()` when a download fails with `ErrDiskFull` (deterministic signal — immediate, no sliding window). Unlike `SKThrottleAccount` and `SKService` which block ALL actions (via `ScopeKey.IsGlobal()`), `SKDiskLocal` blocks downloads only — `ScopeKey.BlocksAction()` returns true only for `ActionDownload`. Uploads, deletes, and moves continue because they either free space or don't consume it. In `ScopeGate.Admit()`, `SKDiskLocal` is checked in priority order between `SKService` and `SKQuotaOwn`. Trial timing uses unified parameters: 5-second initial interval, 2× backoff, 5-minute max cap (computed by `computeTrialInterval()` in engine.go).

### Scanner ScanResult Contract

Implements: R-2.11.5 [implemented], R-2.10.2 [planned]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via two methods:

- **`recordSkippedItems(skipped []SkippedItem)`** — Groups skipped items by reason, batch-upserts to `sync_failures` as actionable failures. Uses aggregated logging: when >10 items share the same reason, logs 1 WARN summary with count and sample paths, individual paths at DEBUG. When <=10 items, logs each as an individual WARN.
- **`clearResolvedSkippedItems(skipped []SkippedItem)`** — Deletes `sync_failures` entries for files that are no longer skipped (e.g., user renamed a previously invalid file). Compares current skipped paths against recorded failures and removes stale entries.

### Aggregated Logging

Implements: R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [planned], R-6.6.10 [verified], R-6.6.12 [verified]

When >10 items share the same warning category, log 1 WARN summary with count and sample paths + individual paths at DEBUG. When <=10 items, log each as an individual WARN. This pattern is implemented in `recordSkippedItems()` for scanner-time validation failures. Transient retries at DEBUG, resolved at INFO, exhausted at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

### Local Permission Handling

Implements: R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.10 [verified]

`os.ErrPermission` → check parent directory accessibility via `handleLocalPermission()`. Inaccessible directory: one `local_permission_denied` at directory level with `SKPermDir(path)` scope block, suppress operations under it. Accessible directory: file-level failure. Recheck directory-level issues at start of each sync pass via `recheckLocalPermissions()`.

**Scanner-driven auto-clear** (R-2.10.10): `clearScannerResolvedPermissions()` checks whether the scanner observed paths that were previously blocked by `local_permission_denied` failures. If the scanner successfully accessed a path (it appeared in events), the permission issue is resolved — clear the failure and release any scope block. File-level: cleared if the path itself was observed. Directory-level (`ScopePermDir` scope): cleared if any observed path falls under the directory prefix (checked via `ScopeKey.IsPermDir()` and `ScopeKey.DirPath()`). Called after `clearResolvedSkippedItems()` in one-shot mode, and after `recheckLocalPermissions()` in watch mode. Complements `recheckLocalPermissions()` — both may clear the same failure (idempotent).

### Planned: Observation Suppression

Implements: R-2.10.30 [verified], R-2.10.31 [verified]

During `SKThrottleAccount` or `SKService` scope block (detected via `ScopeKey.IsGlobal()`), suppress shortcut observation polling (wastes API calls). During `quota:shortcut:*` block, observation continues (read-only).

Observation suppression (`isObservationSuppressed()`) suppresses the entire `processShortcuts()` call, which includes both shortcut discovery and delta polling. Also suppresses `recheckPermissions()` API calls since those are equally wasteful during an outage. Suppressing discovery is acceptable — new shortcuts during an outage would fail immediately anyway. Discovery resumes when the scope clears. Local permission rechecks (`recheckLocalPermissions`) proceed regardless since they are filesystem-only.

**Trial dispatch correctness**: `runTrialDispatch()` uses `AllDueTrials()` snapshot iteration — each due scope is visited exactly once per tick, making infinite iteration structurally impossible. On successful dispatch, the trial interval is NOT mutated (awaiting worker result). On scope-persists, `extendTrialInterval` uses the server's `RetryAfter` if provided (R-2.10.7). The timer re-arms after the trial result via `processTrialResult()` → `armTrialTimer()`.

**Trial path separation**: `processWorkerResult()` checks `IsTrial` and returns early via `processTrialResult()` — trial results never enter the normal switch. This eliminates the prior fragile pattern where trial failures fell through into the normal result switch and required `maybeFeedScopeDetection` guards. `processTrialResult()` handles all trial outcomes self-contained: success releases the scope, failure extends the interval via `extendTrialInterval()`, and scope detection is never called (the scope is already blocked).

**External perm:dir clearance**: `handleExternalChanges()` checks whether `local_permission_denied` failures were cleared via CLI (`issues clear`). Iterates `scopeGate.ScopeBlockKeys()`, filters via `ScopeKey.IsPermDir()`, and releases cleared blocks via `onScopeClear()`.

**Watch mode summary**: `logWatchSummary()` logs a periodic one-liner at the recheck interval (10s) showing actionable issue counts by type. Only logs when the count changes to avoid noisy output.

### Failure Logging (R-6.6.8, R-6.6.10, R-6.6.12)

Implements: R-6.6.8 [verified], R-6.6.10 [verified], R-6.6.12 [verified]

Sync failure logging follows a tiered approach matching CLAUDE.md policy — individual items at DEBUG, aggregated summaries at WARN:

- **Per-failure DEBUG**: `recordFailure()` logs each failure with path, action, HTTP status, error, and scope_key. This is the per-item detail (matching CLAUDE.md Debug = "file read/write").
- **Scope block WARN**: `applyScopeBlock()` logs when a scope block activates with scope_key, issue_type, and trial_interval. This is a degraded-but-recoverable event (matching CLAUDE.md Warn).
- **Scope release INFO**: `processTrialResult()` logs when a scope block clears. This is a lifecycle state transition (matching CLAUDE.md Info).
- **Trial failure DEBUG**: `processTrialResult()` logs failed trials with scope_key and new_interval. This is retry detail.
- **End-of-pass summary**: `logFailureSummary()` aggregates syncErrors by error message prefix. Groups with >10 items get one WARN with count + 3 samples. Groups with ≤10 items get per-item WARN. Mirrors the scanner aggregation in `recordSkippedItems()` (R-6.6.7). Called at end of `executePlan()`.
- **IssueType population**: `recordFailure()` derives issue_type from HTTP status via `issueTypeForHTTPStatus()` and stores it in sync_failures for display grouping.

### Shortcut Integration (`engine_shortcuts.go`)

Detects shortcuts to shared folders in the delta stream. Creates additional delta scopes for shared folder observation. Handles shortcut removal (cleanup local copies).

## Orchestrator (`orchestrator.go`)

Multi-drive coordination. Runs one `DriveRunner` per configured, non-paused drive. Each drive gets its own goroutine, state DB, and sync engine instance. Engines do not coordinate scope blocks across engine boundaries — each engine discovers independently. Bounded waste accepted (one request per engine for 429). Implements: R-2.10.35 [planned]

Pause semantics are delegated to `config.Drive.IsPaused()` and `config.ClearExpiredPauses()` — the orchestrator is a consumer, not an implementor, of pause logic. The initial RunWatch loop checks `ResolvedDrive.Paused` (which is expiry-aware). On SIGHUP reload, `ClearExpiredPauses` clears stale keys before `ResolveDrives` determines the active set. When a timed pause expires during reload, the config keys are cleaned up but the already-running drive is NOT stopped and restarted — avoiding unnecessary downtime.

Handles:
- Drive add/remove via SIGHUP config reload
- Pause/resume per drive (via config package pause APIs)
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
2. Non-delete actions continue to DepGraph and execute normally
3. Held deletes are recorded as `sync_failures` rows with `issue_type=big_delete_held` via `UpsertActionableFailures()`

**CLI notification**: `issues list` shows held deletes in a dedicated "HELD DELETES" section. User approves via `issues clear --all` (or `issues clear <path>` for individual files).

**External change detection**: A 10-second `recheckTicker` in the `RunWatch()` select loop runs `PRAGMA data_version` to detect CLI writes. When the data version changes, `handleExternalChanges()` queries `ListSyncFailuresByIssueType(IssueBigDeleteHeld)`. If zero rows remain (user cleared them all), calls `counter.Release()`. On the next observation cycle, deletions are re-observed and dispatched normally.

**Startup cleanup**: `RunWatch()` clears stale `big_delete_held` entries from prior daemon sessions, since the in-memory counter resets on restart.

**Force mode**: `--force` skips counter creation (`deleteCounter` stays nil), so no watch-mode big-delete protection applies.

### Rationale

- **Crash recovery requires explicit bridging**: On restart after crash, `ResetInProgressStates` resets `remote_state` items stuck mid-execution to pending, AND creates `sync_failures` entries so the drain-loop retrier's bootstrap sweep can rediscover them. This is necessary because the delta token was already advanced before execution — items that crashed mid-execution won't appear in the next delta response. The planner is idempotent for items that DO appear in observations, but crash recovery items need the `sync_failures` → retrier → buffer → planner path.
- **Always use Orchestrator, even for single drive**: N=1 means one DriveRunner — same logic, no special case. Prevents "works for N=1 but breaks for N=2" class of bugs.
