# Data Model

> **BLANK-SLATE DESIGN**: This document describes the event-driven sync
> architecture, designed from first principles. See
> [event-driven-rationale.md](event-driven-rationale.md) for the full rationale.

---

## 1. Overview

### Baseline-Only Persistence

The sync database stores **confirmed synced state** and nothing else. The single
`baseline` table records what was true the last time a file was successfully
synced. Everything else --- remote observations from the delta API, local
observations from the filesystem --- exists only as ephemeral in-memory
`ChangeEvent` values that flow through the pipeline and are discarded after
each cycle.

The database is a checkpoint, not a coordination mechanism.

### What the Database Stores

| Table | Purpose | Mutability |
|---|---|---|
| `baseline` | Confirmed synced state per path | Updated atomically after execution |
| `delta_tokens` | Graph API delta cursor per drive | Updated in the same transaction as baseline |
| `conflicts` | Conflict ledger with resolution history | Append on detection, update on resolution |
| `stale_files` | Files excluded by filter changes | Append on detection, remove on user action |
| `upload_sessions` | Crash recovery for large uploads | Created pre-upload, removed post-upload |
| `change_journal` | Debugging audit trail (optional) | Append-only, compacted periodically |
| `config_snapshots` | Filter change detection | Updated on config load |
| `schema_migrations` | Schema version tracking | Updated on startup |

**8 tables.** The dominant table (`baseline`) has 11 columns.

### Single Writer: BaselineManager

All database writes flow through the `BaselineManager`. The observers (remote
and local) produce change events. The planner produces an action plan. The
executor produces outcomes. Only the `BaselineManager` applies outcomes to the
database, in a single atomic transaction at the end of each cycle. Because all
database writes go through the single `BaselineManager`, there is never more
than one writer, avoiding `SQLITE_BUSY` errors by construction.

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

See [accounts.md](accounts.md) for the complete file layout.

### Token File Layout

Token files follow the same naming pattern, stored in the same data directory:

- `token_personal_<email>.json`
- `token_business_<email>.json`

SharePoint drives share the business account's token file (same OAuth session).
Only the state DB is per-drive.

### WAL Mode and Durability

**Engine**: `modernc.org/sqlite` (pure Go, no CGO dependency).

The database uses WAL (Write-Ahead Logging) mode with `synchronous = FULL` for
crash-safe durability. WAL mode allows concurrent readers while the single
`BaselineManager` writer holds the write lock. See section 12 for the full
pragma list.

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
crash. On restart, the sync engine loads the baseline and re-fetches from the
last saved delta token. Because the delta token and baseline updates are
committed in the same transaction, they are always consistent with each other.
At most one cycle's worth of work is repeated.

---

## 3. Baseline Table

The `baseline` table is the core of the sync database. It stores the confirmed
synced state of every tracked file and folder. Each row represents a path that
was successfully synced --- the content hash, size, and modification time that
both sides agreed on at sync time.

```sql
CREATE TABLE baseline (
    -- Primary key: path relative to sync root (NFC-normalized, URL-decoded)
    -- The filename component is always filepath.Base(path), so no separate
    -- name column is needed.
    path            TEXT    PRIMARY KEY,

    -- Identity: server-assigned, used for remote operations and move detection
    drive_id        TEXT    NOT NULL,    -- normalized: lowercase, zero-padded to 16 chars
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),

    -- Per-side hashes (handles SharePoint enrichment natively)
    -- For normal files: local_hash == remote_hash
    -- For enriched files: they diverge, both recorded
    -- For iOS .heic files: remote hash may be unreliable (known API bug)
    local_hash      TEXT,
    remote_hash     TEXT,

    -- Confirmed synced state
    size            INTEGER,
    mtime           INTEGER,    -- local mtime at sync time (Unix nanoseconds)
                                -- OneDrive truncates to whole seconds; stored at full
                                -- nanosecond precision from local FS for fast-check
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),

    -- Remote metadata for conditional operations (If-Match on deletes)
    etag            TEXT
);
```

**11 columns.** Compared to the prior batch-pipeline `items` table (26 columns), this
eliminates 15 columns by storing only confirmed synced state. Two columns from the
original 13-column design were also removed during first-principles review:

- **`name`**: Redundant with `filepath.Base(path)`. Eliminating it removes a
  consistency invariant that could silently break.
- **`ctag`**: Not used by any code path (planner compares hashes, not ctags;
  executor does not use ctag for conditional operations; delta API returns fresh
  ctags in its responses). Can be added back via migration if ever needed.

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

The baseline table has `path` as its primary key for local operations (path
lookups, prefix queries, cascading updates on folder renames). A unique index
on `(drive_id, item_id)` supports remote operations (move detection: when a
delta reports an item ID at a new path, the baseline can locate the old entry
by item ID).

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

## 4. Delta Tokens Table

Stores the Graph API delta query cursor per drive. The delta token is a
first-class piece of sync state that must be persisted across restarts.

```sql
CREATE TABLE delta_tokens (
    drive_id    TEXT    PRIMARY KEY,
    token       TEXT    NOT NULL,      -- opaque delta token from Graph API
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0)  -- last update (Unix nanoseconds)
);
```

**Critical property**: The delta token is saved in the same transaction as
baseline updates. Never separately. If the process crashes after execution but
before commit, the token is not advanced, and the same delta page is re-fetched
on restart (idempotent). This transactional coupling is what makes crash
recovery correct.

On HTTP 410 (token expired), the sync engine deletes the token and falls back
to full enumeration.

---

## 5. Conflict Ledger

Per-file conflict tracking. The `history` column stores a JSON array of
resolution events, keeping a full audit trail without requiring an additional
table.

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

### History Format

```json
[
    {"action": "detected", "at": 1708123456000000000, "by": "auto"},
    {"action": "keep_both", "at": 1708123460000000000, "by": "auto",
     "renamed": "file.conflict.20240217T120000.txt"},
    {"action": "keep_local", "at": 1708200000000000000, "by": "user"}
]
```

---

## 6. Stale Files Ledger

Tracks files that became excluded by filter changes but still exist locally.
The sync engine **never auto-deletes** stale files --- users must explicitly
dispose of each via the CLI.

```sql
CREATE TABLE stale_files (
    id          TEXT    PRIMARY KEY,   -- UUID (RFC 4122)
    path        TEXT    NOT NULL UNIQUE,  -- local file path (unique: one entry per path)
    reason      TEXT    NOT NULL,      -- why it became stale (e.g., "excluded by skip_files pattern *.tmp")
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),  -- Unix nanoseconds
    size        INTEGER                -- file size in bytes (for display)
);
```

The `UNIQUE(path)` constraint prevents duplicate entries when the same file
becomes stale across multiple config changes. On conflict, the existing row's
reason and timestamp are updated.

---

## 7. Upload Sessions

Tracks resumable upload sessions for large files. The `session_url` is
pre-authenticated (no Bearer token needed). Fragments must be 320 KiB
multiples.

```sql
CREATE TABLE upload_sessions (
    id              TEXT    PRIMARY KEY, -- UUID (RFC 4122)
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,               -- null for new file uploads (assigned after completion)
    local_path      TEXT    NOT NULL,    -- source file on local filesystem
    local_hash      TEXT    NOT NULL,    -- QuickXorHash of local file at session start
    session_url     TEXT    NOT NULL,    -- pre-authenticated upload URL from Graph API
    expiry          INTEGER NOT NULL,    -- session expiration (Unix nanoseconds)
    bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
    total_size      INTEGER NOT NULL,    -- total file size in bytes
    created_at      INTEGER NOT NULL CHECK(created_at > 0)  -- Unix nanoseconds
);
```

On startup, expired sessions (`expiry < now`) are cleaned up. Active sessions
are resumed from `bytes_uploaded` only if the local file's current hash matches
`local_hash` (detecting local modifications during the crash window). If the
hash differs, the session is discarded and the file is re-uploaded from scratch
on the next sync cycle.

---

## 8. Change Journal

An optional append-only table that records every change event observed during
sync cycles. Provides a full audit trail for debugging sync issues in
production. Not required for correct operation --- the sync engine works
identically whether the journal is enabled or disabled.

```sql
CREATE TABLE change_journal (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   INTEGER NOT NULL,    -- when the event was observed (Unix nanoseconds)
    source      TEXT    NOT NULL CHECK(source IN ('remote', 'local')),
    change_type TEXT    NOT NULL CHECK(change_type IN ('create', 'modify', 'delete', 'move')),
    path        TEXT    NOT NULL,
    old_path    TEXT,                -- for moves
    item_id     TEXT,                -- for remote events
    hash        TEXT,
    size        INTEGER,
    mtime       INTEGER,            -- observed mtime (useful for debugging time-based issues)
    cycle_id    TEXT                 -- groups events from the same sync cycle
);
```

**Compaction**: Entries older than a configurable retention period (default 30
days) are periodically deleted. The journal is never read during normal sync
operation --- it exists purely for post-hoc debugging and the `onedrive-go
journal` command (future).

---

## 9. Config Snapshots

Detects configuration changes that require stale file tracking. When filter
patterns, sync paths, or other filter-affecting settings change, the sync
engine compares current values against this snapshot to identify newly-stale
files.

```sql
CREATE TABLE config_snapshots (
    key         TEXT    PRIMARY KEY,  -- config key (e.g., "skip_files", "sync_paths")
    value       TEXT    NOT NULL      -- serialized config value
);
```

---

## 10. Indexes and Performance

### Primary Indexes

```sql
-- Move detection: look up baseline entry by server-assigned item_id
CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);

-- Cascading path operations: folder renames update all children by parent_id
CREATE INDEX idx_baseline_parent ON baseline(parent_id);

-- Conflict filtering by resolution status (for `conflicts` CLI command)
CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);

-- Change journal queries by time range (primary debugging axis)
CREATE INDEX idx_journal_timestamp ON change_journal(timestamp);
```

**Indexes NOT included** (evaluated and rejected during first-principles review):

| Candidate | Why excluded |
|-----------|-------------|
| `idx_baseline_path_prefix ON baseline(path)` | Redundant. `path` is the PRIMARY KEY; SQLite's PK B-tree already supports prefix scans (`LIKE 'prefix/%'`) natively. A separate index doubles write overhead for zero query benefit. |
| `idx_conflicts_drive ON conflicts(drive_id)` | Redundant. Each drive has its own database file, so `drive_id` is identical for every row. An index on a constant column has no selectivity. |
| `idx_journal_path ON change_journal(path)` | Excessive write amplification on a high-volume append-only table. When debugging a specific file, filter by timestamp first (indexed), then by path within the bounded result set. |
| `idx_journal_cycle ON change_journal(cycle_id)` | Same concern. Cycle-based queries are rare and can use the timestamp index (cycles are time-bounded). If needed later, add via migration. |

### Performance Guidelines

**WAL checkpointing**: The `BaselineManager` performs a WAL checkpoint after
each commit transaction. Because all writes happen in a single transaction per
cycle, checkpoint frequency matches cycle frequency.

**VACUUM**: Run only on schema migrations, not as routine maintenance. Routine
VACUUM is expensive and provides minimal benefit under steady-state use.

**Prepared statements**: All repeated queries use prepared statements cached
for the lifetime of the database connection. This avoids re-parsing SQL on
every call.

**Batch operations**: The `BaselineManager.Commit()` applies all outcomes in a
single transaction, minimizing fsync overhead. A typical cycle with 500
changed files produces one transaction with 500 INSERT/UPDATE/DELETE
statements.

**Path-prefix queries**: The baseline table's PRIMARY KEY on `path` supports
efficient prefix matching for queries like `SELECT * FROM baseline WHERE path
LIKE 'Documents/Reports/%'`. SQLite's B-tree ordering on TEXT keys makes
prefix scans O(log n + k) where k is the number of matching rows. No
additional index is needed.

---

## 11. Three-Way Merge Data Flow

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
| Yes | Yes | No | **Conflict**: record in conflict ledger, apply resolution policy |

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
state. The `BaselineManager` applies it:

- **Download completed**: Upsert baseline row with the remote's hash as
  `remote_hash` and the computed local hash as `local_hash` (usually equal,
  unless enrichment occurred).
- **Upload completed**: Upsert baseline row with the local hash as
  `local_hash` and the API-returned hash as `remote_hash`.
- **Delete completed**: Delete the baseline row.
- **Move completed**: Delete the old path's baseline row and insert at the new path.
- **False conflict**: Update the baseline row's hashes, size, and mtime to
  the converged values.

All outcomes are applied in a single transaction together with the new delta
token.

---

## 12. Conventions

### SQLite Pragmas

```sql
PRAGMA journal_mode = WAL;              -- concurrent readers + single writer
PRAGMA synchronous = FULL;              -- durability on crash
PRAGMA foreign_keys = ON;               -- enforce referential integrity
PRAGMA busy_timeout = 5000;             -- 5s wait on lock contention (defense-in-depth)
PRAGMA journal_size_limit = 67108864;   -- 64 MiB WAL size limit
```

The `busy_timeout` is defense-in-depth. Because all database writes go through
the single `BaselineManager`, lock contention is not expected during normal
operation. The timeout protects against unexpected concurrent access (e.g.,
`status` command reading while sync writes).

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

Complete DDL in execution order:

```sql
-- ============================================================
-- Pragmas (set on every connection open, before any queries)
-- ============================================================

PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA journal_size_limit = 67108864;

-- ============================================================
-- Schema migrations tracking
-- ============================================================

CREATE TABLE schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  INTEGER NOT NULL CHECK(applied_at > 0)
);

-- ============================================================
-- Core tables
-- ============================================================

CREATE TABLE baseline (
    path            TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    size            INTEGER,
    mtime           INTEGER,
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
    etag            TEXT
);

CREATE TABLE delta_tokens (
    drive_id    TEXT    PRIMARY KEY,
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0)
);

CREATE TABLE conflicts (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    path            TEXT    NOT NULL,
    conflict_type   TEXT    NOT NULL CHECK(conflict_type IN (
                                'edit_edit', 'edit_delete', 'create_create'
                            )),
    detected_at     INTEGER NOT NULL CHECK(detected_at > 0),
    local_hash      TEXT,
    remote_hash     TEXT,
    local_mtime     INTEGER,
    remote_mtime    INTEGER,
    resolution      TEXT    NOT NULL DEFAULT 'unresolved'
                            CHECK(resolution IN (
                                'unresolved', 'keep_both', 'keep_local',
                                'keep_remote', 'manual'
                            )),
    resolved_at     INTEGER,
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history         TEXT
);

CREATE TABLE stale_files (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL UNIQUE,
    reason      TEXT    NOT NULL,
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),
    size        INTEGER
);

CREATE TABLE upload_sessions (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    local_path      TEXT    NOT NULL,
    local_hash      TEXT    NOT NULL,
    session_url     TEXT    NOT NULL,
    expiry          INTEGER NOT NULL,
    bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
    total_size      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TABLE change_journal (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   INTEGER NOT NULL,
    source      TEXT    NOT NULL CHECK(source IN ('remote', 'local')),
    change_type TEXT    NOT NULL CHECK(change_type IN ('create', 'modify', 'delete', 'move')),
    path        TEXT    NOT NULL,
    old_path    TEXT,
    item_id     TEXT,
    hash        TEXT,
    size        INTEGER,
    mtime       INTEGER,
    cycle_id    TEXT
);

CREATE TABLE config_snapshots (
    key         TEXT    PRIMARY KEY,
    value       TEXT    NOT NULL
);

-- ============================================================
-- Indexes
-- ============================================================

CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);
CREATE INDEX idx_baseline_parent ON baseline(parent_id);
CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);
CREATE INDEX idx_journal_timestamp ON change_journal(timestamp);
```
