# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/tracker.go, internal/sync/scope.go, internal/sync/issue_types.go, internal/sync/compute_status.go, status.go

Implements: R-2.3 [verified], R-5.1 [verified], R-6.4 [implemented], R-6.5.3 [verified], R-6.4.9 [planned], R-6.7.25 [planned], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified], R-2.10.5 [verified], R-2.10.11 [verified], R-2.10.15 [verified], R-2.10.16 [verified], R-2.10.41 [verified], R-2.10.42 [verified], R-2.10.43 [planned], R-2.10.44 [planned]

## Executor (`executor.go`)

Implements: R-6.8.9 [verified]

Thin action dispatcher. Takes an `ActionPlan` and dispatches actions to workers via the DepTracker. Actions dispatched based on dependency satisfaction, not fixed phase ordering. Workers produce individual `Outcome` values committed per-action by the SyncStore.

Action methods call the graph client directly and return `Outcome` — no retry loop, no error classification, no sleep. The following were removed as part of the retry-to-transport refactoring: `withRetry()`, `classifyError()`, `classifyStatusCode()`, `calcExecBackoff()`, `errClass` type + constants, `sleepFunc` field. Hash mismatch retry (`downloadWithHashRetry`) is unchanged — it's a data integrity mechanism orthogonal to the retry redesign.

## Tracker (`tracker.go`)

In-memory dependency graph for action ordering. Folder creates must complete before their children. Depth-first ordering for deletes. Actions are released for execution when all dependencies are satisfied.

### Tracker Extensions

Implements: R-6.8.7 [verified], R-2.10.5 [verified], R-2.10.11 [verified], R-2.10.15 [verified], R-2.10.42 [verified]

- **TrackedAction extensions**: `IsTrial bool` (scope trial probe), `TrialScopeKey string` (which scope a trial is testing — set by `DispatchTrial`, propagated through `WorkerResult`).
- **Held queue**: `held map[string][]*TrackedAction` — per-scope-key map. After dependency resolution, if the action's scope is blocked, it moves to the held queue instead of the worker pool.
- **Scope blocks**: `scopeBlocks map[string]*ScopeBlock` — active scope blocks with trial timing.
- **`HoldScope(key, block)`**: set scope block, future dispatches matching scope go to held queue.
- **`ReleaseScope(key)`**: clear block, dispatch all held actions.
- **`DispatchTrial(key)`**: pop one from held queue, mark `IsTrial=true`, set `TrialScopeKey`, dispatch.
- **`NextDueTrial(now)`**: returns the scope key and `NextTrialAt` of the first scope block whose trial is due.
- **`ExtendTrial(key, nextAt)`**: updates a scope block's `NextTrialAt` and increments its trial attempt counter on trial failure.
- **`dispatch()` gate**: scope gate — blocked actions go to held queue. Unblocked actions reach the ready channel.
- Existing dependency graph behavior unchanged.
- The tracker does NOT handle retry. All retry is via `sync_failures` + `FailureRetrier` (R-6.8.10). The engine calls `Complete` on every result; failed items are recorded in `sync_failures` with `next_retry_at`, and the `FailureRetrier` re-injects them via buffer → planner → tracker.

## Scope Detection (`scope.go`)

Implements: R-2.10.3 [verified], R-2.10.26 [verified], R-2.10.42 [verified]

`ScopeState` maintains sliding windows for scope escalation detection and records successes that reset windows. Thread-safety is provided by the engine's single-goroutine drain loop.

- **Immediate blocks** (server signals): 429 → `throttle:account` (single response triggers). 503 with Retry-After → `service` (single response triggers).
- **Sliding window detection**: 507 → 3 unique paths in 10s → `quota:own` or `quota:shortcut:$key`. 5xx → 5 unique paths in 30s → `service`.
- **400 outage patterns**: `UpdateScopeOutagePattern()` feeds 400 outage patterns (e.g., "ObjectHandle is Invalid") into the service sliding window.
- **Success resets**: `RecordSuccess()` clears sliding windows for the relevant scope — a successful request proves the service is up.

## Worker Pool (`worker.go`)

Implements: R-2.10.16 [verified], R-6.8.12 [verified]

Flat pool of `transfer_workers` goroutines. Workers are pure executors — they execute actions, persist success outcomes, and send `WorkerResult` to the engine. Workers NEVER call `tracker.Complete()` — the engine owns all completion decisions.

`WorkerResult` carries target drive identity (`TargetDriveID`, `ShortcutKey`) from the action, `RetryAfter` from `GraphError`, the full `error` for classification, `ActionID` for tracker routing, `IsTrial` and `TrialScopeKey` for scope trial routing. The engine classifies and routes each result.

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

Issue type constants for failure classification (e.g., `IssueInvalidFilename`, `IssuePathTooLong`, `IssueFileTooLarge`). Moved from the deleted `upload_validation.go`. The upload validation functions (`filterInvalidUploads`, `validateUploadActions`, `validateSingleUpload`, `ValidationFailure`, `removeActionsByIndex`) have been removed entirely — all validation now happens in the observation layer via `shouldObserve()` (Stage 1) and post-stat size checks (Stage 2). See `spec/design/sync-observation.md`.

## Crash Recovery

Implements: R-2.10.41 [verified]

`ResetInProgressStates()` handles crash recovery: on startup, resets items stuck in `downloading`/`deleting` state to `pending_download`/`pending_delete`. In watch mode, `RunWatch` calls `RunOnce` on startup which calls `ResetInProgressStates`, rediscovering all pending items.

Implements: R-2.5.4 [verified]

After resetting `remote_state`, `ResetInProgressStates` also creates corresponding `sync_failures` entries (category=`transient`, direction matching the action, `next_retry_at` computed via `delayFn`) for each item that transitioned to a pending state. This bridges `remote_state` to the sole retry mechanism (`FailureRetrier` + `sync_failures`). Without this bridge, items that crashed mid-execution would become zombies: the delta token was already advanced (no new events), and the `FailureRetrier` only queries `sync_failures`. `RecordFailure` uses UPSERT — if a `sync_failures` entry already exists from a prior failure before the crash, the existing `failure_count` is preserved and incremented, so backoff continues from where it left off.

The `FailureRetrier` (`reconciler.go`) is the sole retry mechanism for sync actions. It periodically sweeps `sync_failures` for items whose `next_retry_at` has expired and re-injects them into the pipeline via buffer → planner → tracker. The engine calls `Complete` on every worker result (never `ReQueue`) and records failures in `sync_failures` with exponential backoff via `retry.Reconcile.Delay`. `CommitOutcome` success cleanup clears `sync_failures` for all action types (upload, download, delete, move).

## Status Computation (`compute_status.go`)

Pure function `computeNewStatus()` determines the new `sync_status` for a `remote_state` row based on the action outcome. Used by `CommitObservation`.

## Planned: Disk Space Pre-Check

Implements: R-2.10.43 [planned], R-2.10.44 [planned], R-6.2.6 [planned]

Pre-check available disk space in executor before download. Two-level check:
- **Critical**: available space < `min_free_space` → `disk:local` scope block, all downloads held until space recovers.
- **Per-file**: available space < file_size + `min_free_space` → per-file failure recorded in `sync_failures`, no scope escalation (other smaller files can still download).

## Design Constraints

- Dotfile conflict naming: `filepath.Ext(".bashrc")` returns `.bashrc`, not `""`. The `conflictStemExt` helper detects single-dot dotfiles and treats extension as empty.
- Delete ordering: depth-first (deepest first), and files before folders at the same depth. `resolveItemType` is a tiebreaker in the sort comparator.
- Ephemeral `Executor` struct per call via `NewExecution(cfg, bl)` — always initializes all mutable fields at construction. Prevents nil-map panics from temporal coupling.
- Edit-delete conflicts (local edit, remote delete) auto-resolve: local modified version wins and is uploaded. Conflict recorded as `resolved_by: "auto"`.
- Individual worker failures increment `report.Failed` and collect errors in `report.Errors`, but `RunOnce()` returns nil. Tests check `report.Failed >= 1`, not `err != nil`.
- Two concurrency knobs: `transfer_workers` (default 8, range 4-64) for file operations, `check_workers` (default 4, range 1-16) for QuickXorHash computation.
- All executor write operations use `containedPath()` with `filepath.IsLocal()` to reject path traversal. Symlink escape from the sync directory is prevented by resolving symlinks on the parent directory via `filepath.EvalSymlinks`.
- Concurrent folder creates via Graph API `$batch` for sibling folders at same depth. Diminishing returns after first sync. [planned]
- Targeted `-race` stress tests for DepTracker, Buffer, WorkerPool. [planned]
- Sub-second uniqueness in `conflictCopyPath`: second-precision timestamps mean two conflicts in the same second collide. [planned]
- Explicit error for unknown `ActionType` in `applySingleOutcome`: default case currently returns nil, silently dropping outcomes. [planned]
- Graceful shutdown test under active worker pool: verify SIGTERM during active transfers drains correctly. [planned]
- Channel lifecycle document: every channel — who creates, closes, reads, writes. [planned]

## CLI Status (`status.go`)

The `status` command reads config, token files, and state databases to display account and drive status. Concurrent-reader safe while sync is running.
