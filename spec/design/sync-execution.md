# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/tracker.go, internal/sync/reconciler.go, internal/sync/issue_types.go, internal/sync/compute_status.go, status.go

Implements: R-2.3 [verified], R-5.1 [verified], R-6.4 [implemented], R-6.5.3 [verified], R-6.4.9 [planned], R-6.7.25 [planned], R-6.8.7 [planned], R-6.8.8 [planned], R-6.8.9 [planned], R-2.10.5 [planned], R-2.10.11 [planned], R-2.10.15 [planned], R-2.10.16 [planned], R-2.10.41 [planned], R-2.10.42 [planned], R-2.10.43 [planned], R-2.10.44 [planned]

## Executor (`executor.go`)

Takes an `ActionPlan` and dispatches actions to workers via the DepTracker. Actions dispatched based on dependency satisfaction, not fixed phase ordering. Workers produce individual `Outcome` values committed per-action by the SyncStore.

**Planned: Thin Action Dispatcher** — Executor will become a thin action dispatcher. `withRetry`, `classifyError`, `classifyStatusCode`, `sleepFunc`, and `errClass` constants will be removed. Action methods call graph client directly and return `Outcome`. The engine classifies errors and schedules retries. Hash mismatch retry (`downloadWithHashRetry`) is unchanged — it's a data integrity mechanism orthogonal to the retry redesign.

## Tracker (`tracker.go`)

In-memory dependency graph for action ordering. Folder creates must complete before their children. Depth-first ordering for deletes. Actions are released for execution when all dependencies are satisfied.

### Planned: Tracker Extensions

Implements: R-6.8.7 [planned], R-2.10.5 [planned], R-2.10.11 [planned], R-2.10.15 [planned], R-2.10.42 [planned]

- **TrackedAction extensions**: `NotBefore time.Time`, `Attempt int`, `MaxAttempts int` (default 5), `IsTrial bool`.
- **Delayed queue**: min-heap ordered by `NotBefore`. Timer goroutine dispatches actions when their delay expires.
- **Held queue**: per-scope-key map. After dependency resolution, if the action's scope is blocked, it moves to the held queue instead of the worker pool.
- **`ReQueue()`**: increment attempt, set `NotBefore` per backoff schedule (1s, 2s, 4s, 8s, 16s), re-enter the dispatch pipeline.
- **`releaseScope()`**: release all held actions for a scope key with `NotBefore = now`.
- **`dispatchTrial()`**: release one action marked `IsTrial` from the held queue for scope block recovery probing.
- Existing dependency graph behavior unchanged.

## Worker Pool (`worker.go`)

Flat pool of `transfer_workers` goroutines. Each worker picks an action, executes it (download/upload/delete/conflict resolution), and returns an Outcome. No lane-based architecture — simplified flat pool.

**Planned: Scope-Aware WorkerResult** — Workers will populate `WorkerResult` with `TargetDriveID` and `ShortcutKey` from the action, plus `RetryAfter` from `GraphError`, so the engine knows which scope the result belongs to. Implements: R-2.10.16 [planned]

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

## Reconciler (`reconciler.go`)

Level-triggered reconciler for crash recovery and failure retry. On startup, resets items stuck in `downloading`/`deleting` state to `pending_download`/`pending_delete`. Schedules retries for failed items using exponential backoff.

**Planned: CommitOutcome Success Cleanup Extension** — `CommitOutcome` success cleanup will be extended to clear `sync_failures` for download/delete/move successes (currently upload-only). Implements: R-2.10.41 [planned]

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
