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
Hash-before-delete guard for local deletions (verifies the file hasn't changed since planning). Remote deletes use `If-Match` with eTag. Local deletes go to OS trash if configured.

### Conflicts (`executor_conflict.go`)
Default: keep both versions. Remote version at original path, local version renamed to `<name>.conflict-<timestamp>.<ext>`. Conflict recorded in `conflicts` table.

## Upload Validation (`upload_validation.go`)

After upload, compares server-reported hash with local hash. If they differ (SharePoint enrichment), records the server hash as `remote_hash` in baseline without re-uploading.

## Reconciler (`reconciler.go`)

Level-triggered reconciler for crash recovery and failure retry. On startup, resets items stuck in `downloading`/`deleting` state to `pending_download`/`pending_delete`. Schedules retries for failed items using exponential backoff.

## Status Computation (`compute_status.go`)

Pure function `computeNewStatus()` determines the new `sync_status` for a `remote_state` row based on the action outcome. Used by `CommitObservation`.

## CLI Status (`status.go`)

The `status` command reads config, token files, and state databases to display account and drive status. Concurrent-reader safe while sync is running.
