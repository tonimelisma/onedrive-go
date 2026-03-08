# Data Model

> **BLANK-SLATE DESIGN**: This document describes the event-driven sync
> architecture, designed from first principles. See
> [event-driven-rationale.md](event-driven-rationale.md) for the full rationale.

---

## 1. Overview

### Remote State Separation (Phase 5.7+)

The sync database uses a **three-table state model** to provide robust failure
recovery and offline verification. See
[remote-state-separation.md](remote-state-separation.md) for the full
architectural rationale.

| Table | Purpose | Mutability |
|---|---|---|
| `remote_state` | Full mirror of remote drive state (every item the delta API has told us about) | Updated on each delta observation and action completion |
| `baseline` | Confirmed synced state per `(drive_id, item_id)` | Updated per-action as each transfer completes |
| `sync_failures` | Unified failure tracking (download, upload, delete) with retry scheduling | Updated on failure, cleared on success |
| `delta_tokens` | Graph API delta cursor per drive | Committed atomically with `remote_state` observations |
| `conflicts` | Conflict tracking with resolution status | Append on detection, update on resolution |
| `sync_metadata` | Key-value store for sync reporting | Updated after each sync cycle |
| `shortcuts` | Shortcut-to-shared-folder tracking | Updated on delta observation |
| `schema_migrations` | Schema version tracking | Updated on startup |

**8 tables.** The three core tables (`remote_state`, `baseline`, `sync_failures`)
implement the Remote State Separation architecture. The `local_issues` table
was replaced by `sync_failures` in migration 00005. Upload sessions are tracked
via a file-based `SessionStore` (JSON files in the data directory), not in the
database.

### SyncStore: Sub-Interface Database Access

All database writes flow through the `SyncStore`, which exposes typed
sub-interfaces grouped by caller identity. Each caller receives the narrowest
interface it needs, enforcing transition ownership at compile time:

| Interface | Caller | Purpose |
|-----------|--------|---------|
| `ObservationWriter` | Remote observer | Writes observed remote state + advances delta token atomically |
| `OutcomeWriter` | Worker pool | Commits action results to baseline + updates `remote_state` on success |
| `FailureRecorder` | `drainWorkerResults` | Records failure metadata in `sync_failures` and transitions `remote_state` status |
| `ConflictEscalator` | Reconciler | Escalates permanently-failing items to conflicts |
| `StateReader` | Reconciler, planner, CLI | Read-only queries across all tables |
| `StateAdmin` | CLI commands, maintenance | Admin writes (resolve conflicts, reset failures) |

Concurrency safety comes from SQLite WAL mode with a 5-second busy timeout.
Every write method uses optimistic concurrency WHERE clauses to prevent stale
updates. See [remote-state-separation.md §10-12](remote-state-separation.md)
for the full concurrency model.

---

## 2. Database Lifecycle

### One Database per Drive

Each configured drive gets its own SQLite database file, providing complete
isolation between drives. The canonical drive identifier (see
[accounts.md](accounts.md)) determines the filename, with `:` replaced by `_`:

- **Linux**: `~/.local/share/onedrive-go/state_<type>_<email>[_<site>_<library>].db`
- **macOS**: `~/Library/Application Support/onedrive-go/state_<type>_<email>[_<site>_<library>].db`

Examples:
- `state_personal_toni@outlook.com.db`
- `state_business_alice@contoso.com.db`
- `state_sharepoint_alice@contoso.com_marketing_Documents.db`
- `state_shared_me@outlook.com_b!TG9yZW0_01ABCDEF.db`

Shared drives follow the same pattern — the canonical ID's `:` separators are replaced with `_` in the filename.

See [accounts.md](accounts.md) for the complete file layout.

### Token File Layout

Token files follow the same naming pattern, stored in the same data directory:

- `token_personal_<email>.json`
- `token_business_<email>.json`

SharePoint drives share the business account's token file (same OAuth session).
Only the state DB is per-drive.

Shared drives share the token with their primary drive (personal or business). Token resolution is handled by `config.TokenCanonicalID()`, which determines the account type by scanning configured drives for the same email. For example, a shared drive `shared:me@outlook.com:b!TG9yZW0:01ABCDEF` uses `token_personal_me@outlook.com.json` if the user has a personal drive configured.

### WAL Mode and Durability

**Engine**: `modernc.org/sqlite` (pure Go, no CGO dependency).

The database uses WAL (Write-Ahead Logging) mode with `synchronous = FULL` for
crash-safe durability. WAL mode allows concurrent readers while writers
serialize via busy timeout. See section 9 for the full pragma list.

### Migration Strategy

Schema migrations use a `schema_migrations` table to track applied versions:

- **Migration files**: Sequential `.sql` files embedded via Go's `embed.FS`
- **Version tracking**: The `schema_migrations` table records which versions
  have been applied and when
- **Forward-only**: Migrations are applied in order on startup. Rollback is
  handled by restoring database backups, not by down-migrations
- **Destructive migrations**: The engine backs up the DB file before running
  migrations that alter existing tables

### First Run

On first run with a new drive:

1. Create the database file and parent directories
2. Set SQLite pragmas (WAL, synchronous, foreign keys, busy timeout)
3. Run all pending migrations to create the schema
4. The database is ready for use

### Crash Recovery

SQLite WAL mode ensures the database is always in a consistent state after a
crash. On restart, the sync engine loads the baseline and queries
`remote_state` for unreconciled rows (items in `downloading`, `pending_download`,
or `download_failed` status). The reconciler resets in-flight items
(`downloading` -> `pending_download`) and schedules retries. The delta token
always reflects the latest successful API poll (not sync success), so no
observations are ever lost. Previously-failed items persist in `remote_state`
and are recovered by the reconciler, not by delta replay. See
[remote-state-separation.md §19](remote-state-separation.md) for the full
crash recovery matrix.

Upload sessions for large files are tracked via file-based `SessionStore` and
can be resumed if the session has not expired and the local file has not
changed.

---

## 3. Remote State Table

The `remote_state` table is a full mirror of every item the delta API has told
us about. It persists remote observations durably, decoupling the delta token
(API cursor) from sync success. See
[remote-state-separation.md §5](remote-state-separation.md) for the rationale.

```sql
CREATE TABLE remote_state (
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL,
    parent_id     TEXT,
    item_type     TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash          TEXT,
    size          INTEGER,
    mtime         INTEGER,
    etag          TEXT,
    previous_path TEXT,
    sync_status   TEXT    NOT NULL DEFAULT 'pending_download'
                  CHECK(sync_status IN (
                      'pending_download', 'downloading', 'download_failed',
                      'synced',
                      'pending_delete', 'deleting', 'delete_failed', 'deleted',
                      'filtered')),
    observed_at   INTEGER NOT NULL CHECK(observed_at > 0),
    PRIMARY KEY (drive_id, item_id)
);
```

**11 columns.** Failure metadata (`failure_count`, `next_retry_at`, `last_error`,
`http_status`) was moved to the `sync_failures` table and removed from
`remote_state` in migration 00006 (B-336).

The `sync_status` column is an explicit state machine that
tracks each item's sync lifecycle. See
[remote-state-separation.md §7](remote-state-separation.md) for the full
state machine and transition ownership.

Key indexes:
- `idx_remote_state_status` on `sync_status` — fast reconciler queries
- `idx_remote_state_parent` on `parent_id` — folder rename cascade
- `idx_remote_state_active_path` — partial unique index on `path` for active
  (non-deleted) items only, preventing path collisions while allowing deleted
  items to retain their path for diagnostics

---

## 4. Baseline Table

The `baseline` table stores the confirmed synced state of every tracked file
and folder. Each row represents an item that was successfully synced --- the
content hash, size, and modification time that both sides agreed on at sync
time. Uses `(drive_id, item_id)` as primary key with `path` as a unique
secondary key.

```sql
CREATE TABLE baseline (
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    path            TEXT    NOT NULL UNIQUE,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),

    -- Per-side hashes (handles SharePoint enrichment natively)
    local_hash      TEXT,
    remote_hash     TEXT,

    -- Confirmed synced state
    size            INTEGER,
    mtime           INTEGER,    -- local mtime at sync time (Unix nanoseconds)
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),

    -- Remote metadata for conditional operations (If-Match on deletes)
    etag            TEXT,
    PRIMARY KEY (drive_id, item_id)
);
```

**11 columns.** ID-based primary key decouples remote identity from local
filesystem paths. Moves are a single UPDATE (atomic) rather than DELETE+INSERT.
Path is a UNIQUE secondary key for fast local lookups.

### Per-Side Hashes

The `local_hash` and `remote_hash` columns are the key innovation for handling
SharePoint enrichment. When SharePoint modifies a file server-side (e.g.,
thumbnail injection into Office documents), the remote hash changes but the
local content has not changed. By recording both hashes at sync time:

- **Normal files**: `local_hash == remote_hash`. Change detection works
  identically to a single-hash model.
- **Enriched files**: `local_hash != remote_hash`. The planner compares
  the new remote hash against `remote_hash` (not `local_hash`) to detect
  genuine remote edits, and compares the new local hash against `local_hash`
  to detect genuine local edits. No false conflicts from enrichment.
- **iOS `.heic` files**: The remote hash from the Graph API is known to be
  unreliable for these files. The per-side model accommodates this by
  allowing `remote_hash` to differ from `local_hash` without triggering
  conflict resolution.

### Dual-Key Access

The baseline table has `(drive_id, item_id)` as its primary key (matching API
identity) and `path` as a UNIQUE secondary key for local operations (path
lookups, prefix queries). Move detection uses the primary key: when a delta
reports an item ID at a new path, the baseline locates the old entry by item ID
and updates the path in a single UPDATE (no DELETE+INSERT).

### Column Notes

| Column | Nullable? | Why |
|--------|-----------|-----|
| `parent_id` | Yes | Root items have no parent |
| `local_hash` | Yes | Folders have no content hash |
| `remote_hash` | Yes | Folders have no content hash |
| `size` | Yes | Folders have no meaningful size |
| `mtime` | Yes | Root items may lack a meaningful mtime |
| `etag` | Yes | Business/SharePoint root items omit eTag |

---

## 5. Delta Tokens Table

Stores the Graph API delta query cursor per drive scope. The delta token is a
first-class piece of sync state that must be persisted across restarts.

```sql
CREATE TABLE delta_tokens (
    drive_id    TEXT    NOT NULL,     -- the configured drive's normalized ID
    scope_id    TEXT    NOT NULL,     -- "" for primary, remoteItem.id for shortcuts
    scope_drive TEXT    NOT NULL,     -- same as drive_id for primary, remoteItem.driveId for shortcuts
    token       TEXT    NOT NULL,     -- opaque delta token from Graph API
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0),  -- last update (Unix nanoseconds)
    PRIMARY KEY (drive_id, scope_id)
);
```

Stores the Graph API delta query cursor per drive scope. Each configured drive
has at least one delta token (the primary scope with `scope_id = ""`). Drives
containing shortcuts to shared folders have additional delta tokens — one per
shortcut, where `scope_id` is the `remoteItem.id` and `scope_drive` is the
`remoteItem.driveId` from the shortcut's remote reference.

**Critical property**: The delta token is committed atomically with
`remote_state` observations in a single transaction via
`SyncStore.CommitObservation()`. The token is a pure API cursor — it tracks
what the API has told us, not what we've synced. This decoupling is the core
of the Remote State Separation design. If the daemon crashes after the
transaction, previously-observed items persist in `remote_state` and are
recovered by the reconciler.

On HTTP 410 (token expired), the sync engine deletes the token and falls back
to full enumeration.

---

## 6. Conflict Tracking

Per-file conflict tracking. The `history` column is reserved for a future
resolution audit trail but is currently dormant/unused (B-160).

```sql
CREATE TABLE conflicts (
    id              TEXT    PRIMARY KEY,  -- UUID (RFC 4122)
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    path            TEXT    NOT NULL,     -- file path at time of conflict detection
    conflict_type   TEXT    NOT NULL CHECK(conflict_type IN (
                                'edit_edit', 'edit_delete', 'create_create'
                            )),
    detected_at     INTEGER NOT NULL CHECK(detected_at > 0),  -- Unix nanoseconds
    local_hash      TEXT,                 -- QuickXorHash of local version (Base64)
    remote_hash     TEXT,                 -- QuickXorHash of remote version (Base64)
    local_mtime     INTEGER,             -- local mtime at conflict (Unix nanoseconds)
    remote_mtime    INTEGER,             -- remote mtime at conflict (Unix nanoseconds)
    resolution      TEXT    NOT NULL DEFAULT 'unresolved'
                            CHECK(resolution IN (
                                'unresolved', 'keep_both', 'keep_local',
                                'keep_remote', 'manual'
                            )),
    resolved_at     INTEGER,             -- resolution timestamp (Unix nanoseconds)
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history         TEXT                  -- JSON array of resolution events
);
```

The `conflict_type` column records what the planner classified (EF5: edit-edit,
EF9: edit-delete, EF12: create-create), providing context for the `conflicts`
CLI command and debugging.

### Resolution Values

| Value | Meaning |
|-------|---------|
| `unresolved` | Conflict detected, not yet resolved |
| `keep_both` | Both versions preserved (loser renamed with conflict suffix) |
| `keep_local` | Local version wins, remote overwritten |
| `keep_remote` | Remote version wins, local overwritten |
| `manual` | User manually resolved via `resolve` command |

---

## 7. Sync Failures Table

Unified failure tracking for all sync directions (download, upload, delete).
Replaced the `local_issues` table and the dead failure columns on
`remote_state` in migration 00005. See
[retry-architecture.md](retry-architecture.md) for the retry design.

```sql
CREATE TABLE sync_failures (
    path           TEXT    NOT NULL,
    drive_id       TEXT    NOT NULL,
    direction      TEXT    NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
    category       TEXT    NOT NULL CHECK(category IN ('transient', 'permanent')),
    issue_type     TEXT,
    item_id        TEXT,
    failure_count  INTEGER NOT NULL DEFAULT 0,
    next_retry_at  INTEGER,
    last_error     TEXT,
    http_status    INTEGER,
    first_seen_at  INTEGER NOT NULL,
    last_seen_at   INTEGER NOT NULL,
    file_size      INTEGER,
    local_hash     TEXT,
    PRIMARY KEY (path, drive_id)
);
```

**14 columns.** Keyed by `(path, drive_id)`. Transient failures use exponential
backoff via `next_retry_at`; permanent failures (`invalid_filename`,
`path_too_long`, `file_too_large`) have `next_retry_at = NULL` and are never
retried. Surfaced via `onedrive-go failures`.

---

## 8. Upload Sessions (File-Based)

Resumable upload sessions for large files are tracked via the `SessionStore`,
which persists session state as individual JSON files in the data directory
(not in the database). Each session file records the upload URL, byte offset,
file hash, and expiry. The `SessionStore` provides `Save`, `Load`, `Delete`,
and `CleanStale` operations with atomic writes (write-to-temp + rename).

On startup, expired sessions are cleaned up. Active sessions are resumed only
if the local file's current hash matches the hash recorded at session start
(detecting local modifications during the crash window). If the hash differs,
the session is discarded and the file is re-uploaded from scratch on the next
sync cycle.

---

## 9. Indexes and Performance

### 9.1 Primary Indexes

```sql
-- Baseline: cascading path operations (folder renames update children by parent_id)
CREATE INDEX idx_baseline_parent ON baseline(parent_id);

-- Remote state: fast reconciler queries
CREATE INDEX idx_remote_state_status ON remote_state(sync_status);
CREATE INDEX idx_remote_state_parent ON remote_state(parent_id);

-- Remote state: path uniqueness for active items only (deleted items retain paths)
CREATE UNIQUE INDEX idx_remote_state_active_path
    ON remote_state(path)
    WHERE sync_status NOT IN ('deleted', 'pending_delete');

-- Conflict filtering by resolution status
CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);
```

**Indexes NOT included** (evaluated and rejected):

| Candidate | Why excluded |
|-----------|-------------|
| `idx_conflicts_drive ON conflicts(drive_id)` | Redundant. Each drive has its own database file, so `drive_id` is identical for every row. |

### 9.2 Performance Guidelines

**WAL checkpointing**: The `SyncStore` performs periodic WAL checkpoints —
after initial sync, every 30 minutes, and on shutdown. This prevents unbounded
WAL growth with the full remote mirror.

**VACUUM**: Run only on schema migrations, not as routine maintenance.

**Prepared statements**: All repeated queries use prepared statements cached
for the lifetime of the database connection.

**Per-action commits**: The `SyncStore` commits each completed action
individually. Each per-action transaction updates the baseline row and the
corresponding `remote_state` row, minimizing fsync overhead per commit while
ensuring incremental progress is durable.

---

## 10. Three-Way Merge Data Flow

The planner constructs a `PathView` for each changed path by combining the
in-memory baseline snapshot with change events. This `PathView` is the input
to the planner's decision matrix.

### PathView Construction

```
For each changed path:
    baseline = Baseline.ByPath[path]      -- may be nil (new item)
    remote   = derive from remote events  -- may be nil (no remote change)
    local    = derive from local events   -- may be nil (no local change)
    view     = PathView{Path, Remote, Local, Baseline}
```

### Change Detection

The planner compares observed state against the baseline to determine what
changed on each side:

```
Remote changed? = (remote.Hash != baseline.RemoteHash)
                  OR (remote.Size != baseline.Size)
                  OR remote.IsDeleted

Local changed?  = (local.Hash != baseline.LocalHash)
                  OR (local.Size != baseline.Size)
```

Note the per-side hash comparison: remote events are compared against
`baseline.RemoteHash`, local events against `baseline.LocalHash`. This is
what makes SharePoint enrichment handling correct by construction.

### Decision Matrix

| Local Changed? | Remote Changed? | Hashes Equal? | Action |
|:-:|:-:|:-:|--------|
| No | No | -- | No action (in sync) |
| No | Yes | -- | Pull: download remote to local |
| Yes | No | -- | Push: upload local to remote |
| Yes | Yes | Yes | False conflict: both sides converged. Update baseline. |
| Yes | Yes | No | **Conflict**: record in conflicts table, apply resolution policy |

### False Conflict Detection

A false conflict occurs when both sides changed independently but arrived at
the same content:

```
Both changed AND (local.Hash == remote.Hash)
```

This is the same pattern used by rclone bisync and Unison. False conflicts are
resolved silently by updating the baseline to match both sides.

### After Successful Sync

After a file is successfully synced (upload verified by hash, download
hash-checked), the executor produces an `Outcome` containing the confirmed
state. The `SyncStore` applies it via `CommitOutcome()`:

- **Download completed**: Upsert baseline row with the remote's hash as
  `remote_hash` and the computed local hash as `local_hash` (usually equal,
  unless enrichment occurred).
- **Upload completed**: Upsert baseline row with the local hash as
  `local_hash` and the API-returned hash as `remote_hash`.
- **Delete completed**: Delete the baseline row.
- **Move completed**: Delete the old path's baseline row and insert at the new path.
- **False conflict**: Update the baseline row's hashes, size, and mtime to
  the converged values.

Each outcome is committed individually as actions complete. The
`CommitOutcome()` transaction also updates the corresponding `remote_state`
row (e.g., setting `sync_status = 'synced'` for downloads, or updating
hash/etag for uploads).

---

## 11. Conventions

### SQLite Pragmas

```sql
PRAGMA journal_mode = WAL;              -- concurrent readers + single writer
PRAGMA synchronous = FULL;              -- durability on crash
PRAGMA foreign_keys = ON;               -- enforce referential integrity
PRAGMA busy_timeout = 5000;             -- 5s wait on lock contention (defense-in-depth)
PRAGMA journal_size_limit = 67108864;   -- 64 MiB WAL size limit
```

The `busy_timeout` handles concurrent write access from multiple goroutines
(observer writing `remote_state`, workers writing baseline via `CommitOutcome`,
drain writing failure metadata). Under WAL mode, readers never block — only
writers serialize.

### Timestamps

All timestamps are stored as `INTEGER` containing Unix nanoseconds since epoch
(UTC). This provides:

- Strong type enforcement --- no ambiguous string formats
- Compact storage --- 8 bytes per timestamp
- Fast comparisons --- integer comparison, no parsing
- Nanosecond precision --- avoids the racily-clean problem where file changes
  within the same second go undetected

Conversion happens at system boundaries only. Internal code uses `int64`
nanoseconds exclusively.

**Validation rules**:

| Condition | Action |
|---|---|
| `0001-01-01T00:00:00Z` or dates before 1970 | Fall back to `NowNano()` |
| Dates more than 1 year in the future | Fall back to `NowNano()` |
| Fractional seconds from OneDrive | Truncated to whole seconds for comparison |
| Local filesystem nanoseconds | Stored at full precision for fast-check |

**Racily-clean problem**: If a file is modified within the same second as the
last sync, the mtime fast-check is ambiguous. Solution: when
`truncateToSeconds(localMtime) == truncateToSeconds(baselineMtime)`, always
compute the content hash to verify.

### Hashes

QuickXorHash values are stored as Base64-encoded `TEXT`, matching the Graph
API's native format. SHA-256 hashes (Business-only, opportunistic) are not
stored in the baseline but may appear in change events.

### Paths

- All paths are relative to the sync root
- NFC-normalized (required for macOS APFS compatibility)
- URL-decoded (delta API returns URL-encoded names)
- Forward slash as separator (even on Windows, for DB consistency)
- No leading or trailing slashes
- Example: `Documents/Reports/Q4.xlsx`

### Identifiers

UUIDs are stored as `TEXT` (RFC 4122 format). Drive IDs and item IDs are
opaque `TEXT` values. All drive IDs are normalized (lowercase, zero-padded)
before storage to handle inconsistencies in the Graph API.

---

## Appendix A: Full DDL

The canonical schema is in `internal/sync/migrations/00001_consolidated_schema.sql`.
See that file for the authoritative DDL. The schema includes:

- `baseline` — confirmed synced state, PK `(drive_id, item_id)`, UNIQUE on `path`
- `delta_tokens` — Graph API delta cursor per drive scope
- `conflicts` — conflict tracking with resolution status
- `sync_metadata` — key-value store for sync reporting
- `remote_state` — full remote mirror with explicit `sync_status` state machine
- `sync_failures` — unified failure tracking (download, upload, delete)
- `shortcuts` — shortcut-to-shared-folder tracking
- `schema_migrations` — schema version tracking (managed by goose)

Pragmas (set on every connection open):

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA journal_size_limit = 67108864;
```
