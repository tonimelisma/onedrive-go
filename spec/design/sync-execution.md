# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/tracker.go, internal/sync/scope.go, internal/sync/issue_types.go, internal/sync/compute_status.go, status.go

Implements: R-2.3 [verified], R-5.1 [verified], R-6.4 [implemented], R-6.5.3 [verified], R-6.4.9 [planned], R-6.7.25 [planned], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified], R-2.10.5 [verified], R-2.10.11 [verified], R-2.10.15 [verified], R-2.10.16 [verified], R-2.10.41 [verified], R-2.10.42 [verified], R-2.10.43 [verified], R-2.10.44 [verified]

## Executor (`executor.go`)

Implements: R-6.8.9 [verified]

Thin action dispatcher. Takes an `ActionPlan` and dispatches actions to workers via the DepTracker. Actions dispatched based on dependency satisfaction, not fixed phase ordering. Workers produce individual `Outcome` values committed per-action by the SyncStore.

Action methods call the graph client directly and return `Outcome` — no retry loop, no error classification, no sleep. The following were removed as part of the retry-to-transport refactoring: `withRetry()`, `classifyError()`, `classifyStatusCode()`, `calcExecBackoff()`, `errClass` type + constants, `sleepFunc` field. Hash mismatch retry (`downloadWithHashRetry`) is unchanged — it's a data integrity mechanism orthogonal to the retry redesign.

## Tracker (`tracker.go`)

In-memory dependency graph for action ordering. Folder creates must complete before their children. Depth-first ordering for deletes. Actions are released for execution when all dependencies are satisfied.

### Tracker Extensions

Implements: R-6.8.7 [verified], R-2.10.5 [verified], R-2.10.11 [verified], R-2.10.15 [verified], R-2.10.42 [verified]

- **TrackedAction extensions**: `IsTrial bool` (scope trial probe), `TrialScopeKey ScopeKey` (which scope a trial is testing — set by `DispatchTrial`, propagated through `WorkerResult`). `ScopeKey` is a typed struct (see Scope Detection below), not a raw string.
- **Held queue**: `held map[ScopeKey][]*TrackedAction` — per-scope-key map. `ScopeKey` is comparable and usable as a map key. After dependency resolution, if the action's scope is blocked, it moves to the held queue instead of the worker pool.
- **Scope blocks**: `scopeBlocks map[ScopeKey]*ScopeBlock` — active scope blocks with trial timing. `ScopeBlock.Key` is typed `ScopeKey`.
- **`HoldScope(key, block)`**: set scope block, future dispatches matching scope go to held queue.
- **`ReleaseScope(key)`**: clear block, dispatch all held actions.
- **`DispatchTrial(key)`**: pop one from held queue, mark `IsTrial=true`, set `TrialScopeKey`, clear `NextTrialAt` (prevents re-dispatch until trial result re-arms via `armTrialTimer`), dispatch.
- **`NextDueTrial(now)`**: returns the scope key and `NextTrialAt` of the first scope block whose trial is due.
- **`ExtendTrialInterval(key, nextAt, interval)`**: updates a scope block's `NextTrialAt`, `TrialInterval`, and increments its trial attempt counter on trial failure. Encapsulates the mutation under the tracker's lock.
- **`EarliestTrialAt()`**: scans all scope blocks for the earliest `NextTrialAt` with non-empty held queue. Used by the engine's trial timer.
- **`ScopeBlockKeys()`**: returns all active scope block keys. Used by `handleExternalChanges` to detect when `perm:dir` failures have been cleared via CLI.
- **`GetScopeBlock(key)`**: returns a value copy of the `ScopeBlock` for a given scope key (not a pointer, preventing mutation outside the lock). Used by `handleTrialResult` to read current `TrialInterval` for backoff doubling.
- **`dispatch()` gate**: scope gate — blocked actions go to held queue. Returns `bool` (was-held) so callers can fire the `onHeld` callback outside the lock.
- **`onHeld` callback**: called when `dispatch()` diverts an action to a held queue. The engine sets this to `armTrialTimer` so the trial timer re-arms when the held queue becomes non-empty. Must NOT be called under `dt.mu` — callers invoke it after releasing the lock.
- **`blockedScope()` dispatch via `ScopeKey.BlocksAction()`**: Fixed-key scopes are checked in priority order (throttle, service, disk, quota:own), then dynamic-key scopes (shortcut quota, perm:dir) via O(n) scan. Each check delegates to `ScopeKey.BlocksAction(path, shortcutKey, actionType, targetsOwnDrive)` — scope-specific blocking logic lives on the `ScopeKey` type, not in the tracker. Adding a new scope kind requires implementing `BlocksAction` for that kind.
- Existing dependency graph behavior unchanged.
- The tracker does NOT handle retry. All retry is via `sync_failures` + `FailureRetrier` (R-6.8.10). The engine calls `Complete` on every result; failed items are recorded in `sync_failures` with `next_retry_at`, and the `FailureRetrier` re-injects them via buffer → planner → tracker.

## Scope Detection (`scope.go`)

Implements: R-2.10.3 [verified], R-2.10.26 [verified], R-2.10.42 [verified]

### ScopeKey Type System

All scope keys are typed `ScopeKey{Kind ScopeKeyKind, Param string}` — a comparable value type usable as map key. Six kinds: `ScopeThrottleAccount`, `ScopeService`, `ScopeQuotaOwn`, `ScopeQuotaShortcut` (Param = "remoteDrive:remoteItem"), `ScopePermDir` (Param = relative dir path), `ScopeDiskLocal`. Pre-built singletons for non-parameterized scopes (`SKThrottleAccount`, `SKService`, `SKQuotaOwn`, `SKDiskLocal`); constructor functions for parameterized scopes (`SKQuotaShortcut(key)`, `SKPermDir(path)`).

Methods on `ScopeKey` centralize logic that was previously scattered across 9+ files:
- **`BlocksAction(path, shortcutKey, actionType, targetsOwnDrive)`** — scope-specific action blocking (used by `blockedScope()`)
- ~~`MaxTrialInterval()`~~ — removed; interval computation centralized in `computeTrialInterval()` (engine.go)
- **`Humanize(shortcuts)`** — user-friendly description for display
- **`IssueType()`** — maps scope kind to `sync_failures.issue_type` constant
- **`IsGlobal()`** — true for scopes that block ALL actions (throttle, service)
- **`IsPermDir()` / `DirPath()`** — type-safe access for permission scopes
- **`IsZero()`** — detects the zero-value (invalid) key
- **`String()` / `ParseScopeKey(s)`** — wire format serialization for SQLite `scope_key` columns. The wire format is unchanged (`"throttle:account"`, `"service"`, `"quota:own"`, `"quota:shortcut:X"`, `"perm:dir:X"`, `"disk:local"`), preserving compatibility at the SQLite boundary.
- **`ScopeKeyForStatus(httpStatus, shortcutKey)`** — single source of truth for HTTP status → scope key classification, replacing scattered switch/if chains in `classifyResult` and `deriveScopeKey`.

### Scope Escalation

`ScopeState` maintains sliding windows for scope escalation detection and records successes that reset windows. Thread-safety is provided by the engine's single-goroutine drain loop. Windows are keyed by `ScopeKey` (not string).

- **Immediate blocks** (server signals): 429 → `SKThrottleAccount` (single response triggers). 503 with Retry-After → `SKService` (single response triggers).
- **Sliding window detection**: 507 → 3 unique paths in 10s → `SKQuotaOwn` or `SKQuotaShortcut(key)`. 5xx → 5 unique paths in 30s → `SKService`.
- **400 outage patterns**: `UpdateScopeOutagePattern()` feeds 400 outage patterns (e.g., "ObjectHandle is Invalid") into the service sliding window.
- **Success resets**: `RecordSuccess()` clears sliding windows for the relevant scope — a successful request proves the service is up.

## Worker Pool (`worker.go`)

Implements: R-2.10.16 [verified], R-6.8.12 [verified]

Flat pool of `transfer_workers` goroutines. Workers are pure executors — they execute actions, persist success outcomes, and send `WorkerResult` to the engine. Workers NEVER call `tracker.Complete()` — the engine owns all completion decisions.

`WorkerResult` carries target drive identity (`TargetDriveID`, `ShortcutKey`) from the action, `RetryAfter` from `GraphError`, the full `error` for classification, `ActionID` for tracker routing, `IsTrial` and `TrialScopeKey ScopeKey` for scope trial routing. The engine classifies and routes each result.

## Action Execution

### Downloads (`executor_transfer.go`)
`.partial` file + hash verify + atomic rename. Uses `TransferManager` from `driveops`.

### Uploads (`executor_transfer.go`)
Simple PUT (≤4 MiB) or resumable session (>4 MiB). Post-upload validation detects SharePoint enrichment.

### Deletes (`executor_delete.go`)
Implements: R-6.2.4 [verified]

Hash-before-delete guard for local deletions (verifies the file hasn't changed since planning). Remote deletes use `If-Match` with eTag. Local deletes go to OS trash if configured. When a local folder delete would fail due to non-empty directory containing only disposable files (OS junk like `.DS_Store`, editor temps like `.swp`, invalid OneDrive names), `deleteLocalFolder` auto-removes them before retrying the folder delete.

### Conflicts (`executor_conflict.go`)
Default: keep both versions. Remote version at original path, local version renamed to `<name>.conflict-<timestamp>.<ext>`. Conflict recorded in `conflicts` table.

## Issue Types (`issue_types.go`)

Issue type constants for failure classification (e.g., `IssueInvalidFilename`, `IssuePathTooLong`, `IssueFileTooLarge`, `IssueBigDeleteHeld`). Moved from the deleted `upload_validation.go`. The upload validation functions (`filterInvalidUploads`, `validateUploadActions`, `validateSingleUpload`, `ValidationFailure`, `removeActionsByIndex`) have been removed entirely — all validation now happens in the observation layer via `shouldObserve()` (Stage 1) and post-stat size checks (Stage 2). See `spec/design/sync-observation.md`.

`IssueBigDeleteHeld` is an actionable issue type used by the watch-mode big-delete protection (see sync-engine.md). Held delete actions are recorded with this type and displayed in a dedicated "HELD DELETES" section in `issues list`.

## Crash Recovery

Implements: R-2.10.41 [verified]

`ResetInProgressStates()` handles crash recovery: on startup, resets items stuck in `downloading`/`deleting` state to `pending_download`/`pending_delete`. In watch mode, `RunWatch` calls `RunOnce` on startup which calls `ResetInProgressStates`, rediscovering all pending items.

Implements: R-2.5.4 [verified]

After resetting `remote_state`, `ResetInProgressStates` also creates corresponding `sync_failures` entries (category=`transient`, direction matching the action, `next_retry_at` computed via `delayFn`) for each item that transitioned to a pending state. This bridges `remote_state` to the sole retry mechanism (`FailureRetrier` + `sync_failures`). Without this bridge, items that crashed mid-execution would become zombies: the delta token was already advanced (no new events), and the `FailureRetrier` only queries `sync_failures`. `RecordFailure` uses UPSERT — if a `sync_failures` entry already exists from a prior failure before the crash, the existing `failure_count` is preserved and incremented, so backoff continues from where it left off.

The `FailureRetrier` (`reconciler.go`) is the sole retry mechanism for sync actions. It periodically sweeps `sync_failures` for items whose `next_retry_at` has expired and re-injects them into the pipeline via buffer → planner → tracker. The engine calls `Complete` on every worker result (never `ReQueue`) and records failures in `sync_failures` with exponential backoff via `retry.Reconcile.Delay`. `CommitOutcome` success cleanup clears `sync_failures` for all action types (upload, download, delete, move).

**Double-dispatch prevention**: The retrier tracks the last-dispatched `next_retry_at` for each path in `dispatchedRetryAt`. If a row's `NextRetryAt` matches the tracked value, the row was already injected into the buffer and is skipped. This guards against the bootstrap-vs-kick race: when `Run()` starts, the bootstrap reconcile may dispatch a row that arrived between goroutine creation and bootstrap execution, while the kick signal (from `processWorkerResult`) is still pending in the channel. Without this guard, both sweeps dispatch the same row. When `RecordFailure` sets a new `next_retry_at` (re-failure after planner re-evaluation), the mismatch naturally allows re-dispatch. Entries are cleared when the path becomes in-flight (pipeline consumed it). Stale entries (paths whose `sync_failures` rows were resolved or reclassified) are pruned each sweep via `pruneDispatchedRetryAt` to prevent monotonic map growth.

## Status Computation (`compute_status.go`)

Pure function `computeNewStatus()` determines the new `sync_status` for a `remote_state` row based on the action outcome. Used by `CommitObservation`.

## Disk Space Pre-Check

Implements: R-2.10.43 [verified], R-2.10.44 [verified], R-6.2.6 [verified], R-6.4.7 [verified]

Disk space pre-checks now live in `TransferManager.DownloadToFile` (see [drive-transfers.md](drive-transfers.md#disk-space-pre-check)), giving every download caller automatic protection. The sync engine wires the check via `driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable)` when constructing the TransferManager in `NewEngine`.

Config wiring: `min_free_space` string (default "1GB") is parsed via `config.ParseSize()` and threaded from `ResolvedDrive` through `EngineConfig.MinFreeSpace` to `TransferManager` via `WithDiskCheck`. Zero disables the check (R-6.4.7).

Scope block classification remains in the sync engine: `classifyResult` maps `driveops.ErrDiskFull` → `disk:local` scope block, `driveops.ErrFileTooLargeForSpace` → per-file skip.

## Design Constraints

- Dotfile conflict naming: `filepath.Ext(".bashrc")` returns `.bashrc`, not `""`. The `conflictStemExt` helper detects single-dot dotfiles and treats extension as empty.
- Delete ordering: depth-first (deepest first), and files before folders at the same depth. `resolveItemType` is a tiebreaker in the sort comparator.
- Ephemeral `Executor` struct per call via `NewExecution(cfg, bl)` — always initializes all mutable fields at construction. Prevents nil-map panics from temporal coupling.
- Edit-delete conflicts (local edit, remote delete) auto-resolve: local modified version wins and is uploaded. Conflict recorded as `resolved_by: "auto"`.
- Individual worker failures increment `report.Failed` and collect errors in `report.Errors`, but `RunOnce()` returns nil. Tests check `report.Failed >= 1`, not `err != nil`.
- Two concurrency knobs: `transfer_workers` (default 8, range 4-64) for file operations, `check_workers` (default 4, range 1-16) for QuickXorHash computation.
- All executor write operations use `containedPath()` with `filepath.IsLocal()` to confine local filesystem writes to the sync root directory. This is defense-in-depth against path reconstruction bugs, not input validation (the OneDrive API is the source of truth, not an attacker). Symlink escape is prevented by resolving symlinks on the parent directory via `filepath.EvalSymlinks`.
- Concurrent folder creates via Graph API `$batch` for sibling folders at same depth. Diminishing returns after first sync. [planned]
- Targeted `-race` stress tests for DepTracker, Buffer, WorkerPool. [planned]
- Sub-second uniqueness in `conflictCopyPath`: second-precision timestamps mean two conflicts in the same second collide. [planned]
- Explicit error for unknown `ActionType` in `applySingleOutcome`: default case currently returns nil, silently dropping outcomes. [planned]
- Graceful shutdown test under active worker pool: verify SIGTERM during active transfers drains correctly. [planned]
- Channel lifecycle document: every channel — who creates, closes, reads, writes. [planned]

## CLI Status (`status.go`)

The `status` command reads config, token files, and state databases to display account and drive status. Concurrent-reader safe while sync is running.
