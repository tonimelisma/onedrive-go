# Sync Store

GOVERNS: internal/sync/baseline.go, internal/sync/store_interfaces.go, internal/sync/migrations.go, internal/sync/verify.go, internal/sync/trash.go, issues.go, verify.go

Implements: R-2.5 [verified], R-2.3.2 [verified], R-2.3.3 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.7 [verified], R-6.4.4 [verified], R-6.4.5 [verified], R-2.15.1 [verified], R-2.10.1 [planned], R-2.10.2 [planned], R-2.10.33 [implemented], R-2.10.34 [planned], R-2.10.41 [implemented]

## SyncStore (`baseline.go`)

Database access layer exposing typed sub-interfaces. See [data-model.md](data-model.md) for the sub-interface table and schema details.

Key operations:
- `CommitObservation()`: atomically writes `remote_state` rows + advances delta token in a single transaction
- `CommitOutcome()`: updates baseline + `remote_state` status per action
- `RecordFailure(ctx, SyncFailureParams)`: unified failure recording — always transactional, handles all failure types. For download/delete: atomically transitions `remote_state` and records `sync_failures`. Actionable issues get `category='actionable'` with no `next_retry_at`; transient issues get exponential backoff. UPSERT with COALESCE preserves existing `file_size`, `local_hash`, `item_id` on conflict. Auto-resolves `item_id` from `remote_state` for download/delete when not provided

All write methods use optimistic concurrency (WHERE clauses preventing stale updates). Concurrency safety from SQLite WAL mode with 5-second busy timeout. Implements: R-6.3.2 [verified]

## Store Interfaces (`store_interfaces.go`)

Typed sub-interfaces enforce transition ownership at compile time. Each caller receives the narrowest interface it needs. See [data-model.md](data-model.md) for the full interface-to-caller mapping.

`SyncStore.Load()` uses a cache-through pattern: returns cached `*Baseline` if non-nil. `Commit()` invalidates the cache before calling `Load()` to refresh. For N conflict resolutions, this reduces 2N DB loads to 1 initial load + N refreshes.

## Migrations (`migrations.go`)

Embedded `.sql` files via Go `embed.FS`. Applied in order on startup. The `schema_migrations` table tracks versions. DB backed up before destructive migrations.

## Verification (`verify.go`)

`verify` command re-hashes local files and compares against baseline and remote state. Reports discrepancies: missing files, hash mismatches, extra files not in baseline.

## Trash (`trash.go`)

OS trash integration for local deletions triggered by remote changes:
- **macOS**: moves to `~/.Trash/` with collision handling (append numeric suffix)
- **Linux**: returns error (opt-in only, XDG trash not implemented)

Controlled by `use_local_trash` config (default: true on macOS, false on Linux).

## CLI Commands

### Issues (`issues.go`)

`issues` lists unresolved conflicts and failures. Sub-commands: `issues resolve <path>` (keep-local/keep-remote/keep-both), `issues clear <path>` (dismiss), `issues retry <path>` (retry failed item).

**Planned: Issues Display Enhancements** — Grouped display for >10 failures of same type (count + first 5 paths, `--verbose` for all). Per-scope sub-grouping for 507/403 (own drive vs each shortcut). Human-readable shortcut names, not opaque drive IDs. Implements: R-2.3.7 [planned], R-2.3.8 [planned], R-2.3.9 [planned]

### Verify (`verify.go` in root)

CLI wiring for the verification command. Opens state DB read-only, runs verification, displays results.

## SyncStore Planned Improvements

- Audit `ForEachPath` callers for re-entrancy safety: holds read lock during callback — write from callback causes deadlock. [planned]
- Mutex or `sync.Once` on `SyncStore.Load`: concurrent `Load` calls race on `m.baseline = b`. [planned]
- Disk full during baseline commit: in-memory cache consistency when SQLite write fails. [planned]
- Evaluate `BaselineStore` interface abstraction for storage backend flexibility. [planned]

## Planned: Failure Management Enhancements

Implements: R-2.10.1 [planned], R-2.10.2 [planned], R-2.10.33 [implemented], R-2.10.34 [planned], R-2.10.41 [implemented]

**New issue types**: `quota_exceeded`, `local_permission_denied`, `case_collision`, `disk_full`, `service_outage`, `file_too_large_for_space`.

**Scope key column**: `scope_key TEXT NOT NULL DEFAULT ''` added to `sync_failures` table (migration). Format: `quota:own`, `quota:shortcut:{localPath}`, `perm:remote:{localPath}`, `disk:local`, `throttle:account`, `service`. Enables `issues` display grouping without re-deriving scope.

**Store method changes**:
- `RecordFailure(ctx, SyncFailureParams)`: unified method replacing both `RecordSyncFailure` (11-param, non-transactional) and `RecordFailureWithStateTransition` (8-param, transactional). Always transactional, handles state transitions for download/delete as a safe no-op for uploads. `SyncFailureParams` struct bundles all inputs with named fields.
- `UpsertActionableFailures([]ActionableFailure)`: batch upsert for scanner-detected naming/collision issues.
- `ClearResolvedActionableFailures(issueType, currentPaths)`: compare current skipped paths against recorded `sync_failures`; delete entries for paths no longer in the skipped set. Uses `strings.Repeat` for SQL placeholder construction.
- `CommitOutcome`: success cleanup for download/delete/move clears `sync_failures` entries.
- `ListSyncFailuresByIssueType(ctx, issueType)`: added to `SyncFailureRecorder` interface (was concrete-only).
