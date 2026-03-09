# Sync Execution

GOVERNS: internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_transfer.go, internal/sync/worker.go, internal/sync/tracker.go, internal/sync/reconciler.go, internal/sync/upload_validation.go, internal/sync/compute_status.go, status.go

Implements: R-2.3 [implemented], R-5.1 [implemented], R-6.4 [implemented], R-6.5.3 [implemented]

## Executor (`executor.go`)

Takes an `ActionPlan` and dispatches actions to workers via the DepTracker. Actions dispatched based on dependency satisfaction, not fixed phase ordering. Workers produce individual `Outcome` values committed per-action by the SyncStore.

## DepTracker (`tracker.go`)

In-memory dependency graph for action ordering. Folder creates must complete before their children. Depth-first ordering for deletes. Actions are released for execution when all dependencies are satisfied.

## Worker Pool (`worker.go`)

Flat pool of `transfer_workers` goroutines. Each worker picks an action, executes it (download/upload/delete/conflict resolution), and returns an Outcome. No lane-based architecture — simplified flat pool.

## Action Execution

### Downloads (`executor_transfer.go`)
`.partial` file + hash verify + atomic rename. Uses `TransferManager` from `driveops`.

### Uploads (`executor_transfer.go`)
Simple PUT (≤4 MiB) or resumable session (>4 MiB). Post-upload validation detects SharePoint enrichment.

### Deletes (`executor_delete.go`)
Hash-before-delete guard for local deletions (verifies the file hasn't changed since planning). Remote deletes use `If-Match` with eTag. Local deletes go to OS trash if configured. When a local folder delete would fail due to non-empty directory containing only disposable files (OS junk like `.DS_Store`, editor temps like `.swp`, invalid OneDrive names), `deleteLocalFolder` auto-removes them before retrying the folder delete.

### Conflicts (`executor_conflict.go`)
Default: keep both versions. Remote version at original path, local version renamed to `<name>.conflict-<timestamp>.<ext>`. Conflict recorded in `conflicts` table.

## Upload Validation (`upload_validation.go`)

After upload, compares server-reported hash with local hash. If they differ (SharePoint enrichment), records the server hash as `remote_hash` in baseline without re-uploading.

## Reconciler (`reconciler.go`)

Level-triggered reconciler for crash recovery and failure retry. On startup, resets items stuck in `downloading`/`deleting` state to `pending_download`/`pending_delete`. Schedules retries for failed items using exponential backoff.

## Status Computation (`compute_status.go`)

Pure function `computeNewStatus()` determines the new `sync_status` for a `remote_state` row based on the action outcome. Used by `CommitObservation`.

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
