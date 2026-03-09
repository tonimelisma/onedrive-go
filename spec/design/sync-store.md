# Sync Store

GOVERNS: internal/sync/baseline.go, internal/sync/store_interfaces.go, internal/sync/migrations.go, internal/sync/verify.go, internal/sync/trash.go, issues.go, verify.go

Implements: R-2.5 [implemented], R-2.3.2 [implemented], R-2.3.3 [implemented], R-2.7 [implemented], R-6.4.4 [implemented], R-6.4.5 [implemented]

## SyncStore (`baseline.go`)

Database access layer exposing typed sub-interfaces. See [data-model.md](data-model.md) for the sub-interface table and schema details.

Key operations:
- `CommitObservation()`: atomically writes `remote_state` rows + advances delta token in a single transaction
- `CommitOutcome()`: updates baseline + `remote_state` status per action
- `RecordFailure()`: writes to `sync_failures` with retry scheduling

All write methods use optimistic concurrency (WHERE clauses preventing stale updates). Concurrency safety from SQLite WAL mode with 5-second busy timeout. Implements: R-6.3.2 [implemented]

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

### Verify (`verify.go` in root)

CLI wiring for the verification command. Opens state DB read-only, runs verification, displays results.

## SyncStore Planned Improvements

- Audit `ForEachPath` callers for re-entrancy safety: holds read lock during callback — write from callback causes deadlock. [planned]
- Mutex or `sync.Once` on `SyncStore.Load`: concurrent `Load` calls race on `m.baseline = b`. [planned]
- Disk full during baseline commit: in-memory cache consistency when SQLite write fails. [planned]
- Evaluate `BaselineStore` interface abstraction for storage backend flexibility. [planned]

## Planned: Failure Management Enhancements

Implements: R-2.10.1 [planned], R-2.10.2 [planned]

- HTTP 507 (quota exceeded) classification: currently misclassified as transient. Should be actionable with `issue_type='quota_exceeded'`, visible in `issues`, with time-based retry. Requires changes in `upload_validation.go`, `baseline.go`, schema, and engine. [planned]
- Stale actionable failure cleanup: when a user fixes a file-scoped actionable failure (rename, move, delete), the old `sync_failures` row should be automatically detected and removed. Options: `recheckActionableFailures()` at start of pass, scanner deletion detection enhancement, or aggressive pruning. [planned]
