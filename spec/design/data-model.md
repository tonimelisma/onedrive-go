# Data Model

Implements: R-6.5.1 [verified], R-6.5.2 [verified], R-2.5.1 [verified], R-2.5.2 [verified], R-2.3.2 [verified]

## One Database per Drive

Each configured drive gets its own SQLite database file. The canonical drive identifier determines the filename (`:` replaced by `_`):

- **Linux**: `~/.local/share/onedrive-go/state_<type>_<email>[_<site>_<library>].db`
- **macOS**: `~/Library/Application Support/onedrive-go/state_<type>_<email>[_<site>_<library>].db`

Engine: `modernc.org/sqlite` (pure Go, no CGO). WAL mode, `synchronous = FULL`, 5-second busy timeout.

## Three-Table State Model

The sync database uses remote state separation: three core tables decouple API observation from sync success.

| Table | Purpose | Key |
|-------|---------|-----|
| `remote_state` | Full mirror of every item the delta API reports | `(drive_id, item_id)` |
| `baseline` | Confirmed synced state | `(drive_id, item_id)`, `path` UNIQUE |
| `sync_failures` | Unified failure tracking with retry scheduling | `(path, drive_id)` |

Supporting tables: `delta_tokens`, `conflicts`, `sync_metadata`, `shortcuts`, `schema_migrations`.

### remote_state

Full mirror of remote drive state. Updated on each delta observation and action completion. The `sync_status` column is an explicit state machine:

```
pending_download → downloading → synced
                              → download_failed → pending_download (reconciler reset)
pending_delete   → deleting    → deleted
                              → delete_failed
filtered         (terminal — item excluded by filter rules)
```

Items in `downloading` or `deleting` at startup are reset to `pending_download` / `pending_delete` by the reconciler (crash recovery).

11 columns. `previous_path` tracks renames for move detection. Partial unique index on `path` for active items only (deleted items retain paths for diagnostics).

### baseline

Confirmed synced state per `(drive_id, item_id)`. Each row represents content that both local and remote agree on.

Per-side hashes (`local_hash`, `remote_hash`) handle SharePoint enrichment: when SharePoint modifies a file server-side, the remote hash changes but the local content hasn't changed. The planner compares new hashes against the correct side's baseline hash to avoid false conflicts.

`path` is a UNIQUE secondary key for fast local lookups. Moves are a single UPDATE (atomic) rather than DELETE+INSERT, enabled by the ID-based primary key.

Baseline memory footprint is ~19 MB per 100K files, additive across drives. Monitor under profiling. [planned]

Zero-byte files map to `NULL` in SQLite — indistinguishable from "size unknown". Fix: use `sql.NullInt64{Valid: true, Int64: 0}`. [planned]

### sync_failures

Unified failure tracking for download, upload, and delete failures. Two categories:
- `transient` — retried with exponential backoff via `next_retry_at`
- `actionable` — require user intervention (`next_retry_at = NULL`)

14 columns. Keyed by `(path, drive_id)`. Surfaced via the `issues` CLI command.

### delta_tokens

Delta API cursor per drive scope. `scope_id = ""` for primary scope. Drives with shortcuts to shared folders have additional scopes (one per shortcut). The cursor is committed atomically with `remote_state` observations — it tracks what the API has reported, not what has been synced.

### conflicts

Per-file conflict tracking. Three types: `edit_edit`, `edit_delete`, `create_create`. Four resolution states: `unresolved`, `keep_both`, `keep_local`, `keep_remote`, `manual`.

### shortcuts

Shortcut-to-shared-folder registry. Tracks `remote_drive`, `remote_item`, observation method (`delta` or `enumerate`), and `read_only` status.

## SyncStore Sub-Interfaces

All database writes flow through typed sub-interfaces, enforcing transition ownership at compile time:

| Interface | Caller | Purpose |
|-----------|--------|---------|
| `ObservationWriter` | Remote observer | Write observed remote state + advance delta token atomically |
| `OutcomeWriter` | Worker pool | Commit action results to baseline + update remote_state |
| `FailureRecorder` | Worker result drain | Record failure metadata in sync_failures |
| `StateReader` | Reconciler, planner, CLI | Read-only queries across all tables |
| `StateAdmin` | CLI commands | Admin writes (resolve conflicts, reset failures) |

## Upload Sessions (File-Based)

Resumable upload sessions are tracked as JSON files in the data directory (not in the database). On resume, the local file's hash is compared against the hash at session start — if it differs, the session is discarded.

## Migrations

Schema migrations use embedded `.sql` files applied in order on startup. The `schema_migrations` table tracks applied versions. Forward-only — rollback via database backup restore. The engine backs up the DB file before destructive migrations.

## Performance

- **WAL checkpointing**: After initial sync, every 30 minutes, and on shutdown. Explicit `PRAGMA wal_checkpoint(TRUNCATE)` before database close ensures all WAL data is flushed to the main file.
- **Prepared statements**: Cached for connection lifetime
- **Per-action commits**: Each completed action committed individually for incremental durability
- **VACUUM**: On schema migrations only, not routine maintenance
- **Batched commits**: Per-action commit is ~0.5ms. For high-throughput workloads, batched commits could reduce overhead. Currently bottleneck is network I/O, not SQLite. [planned]
