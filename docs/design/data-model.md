# Data Model: onedrive-go

This document specifies the SQLite database schema and state management model for the onedrive-go sync engine. It defines every table, column, index, and constraint that the `internal/state/` package implements.

---

## 1. Overview

### Purpose and Scope

The sync database is the single source of truth for the state of every tracked file and folder. It stores three views of each item — local observed, remote observed, and last-synced — enabling the three-way merge algorithm that underpins bidirectional sync. It also stores delta tokens, conflict records, stale file records, and upload session state.

This document covers:
- SQLite configuration and pragmas
- Database lifecycle and migration strategy
- Complete DDL for all tables and indexes
- Path materialization and tombstone lifecycle algorithms
- Three-way merge data flow
- Timestamp and hash conventions

### Relationship to Architecture

The architecture document ([architecture.md](architecture.md) §6) defines high-level constraints. This document is the concrete implementation of those constraints. Every architectural requirement is traced back in the relevant section.

### SQLite Configuration

```sql
PRAGMA journal_mode = WAL;          -- concurrent readers + single writer
PRAGMA synchronous = FULL;          -- durability on crash
PRAGMA foreign_keys = ON;           -- enforce referential integrity
PRAGMA journal_size_limit = 67108864; -- 64 MiB WAL size limit
```

**Engine**: `modernc.org/sqlite` (pure Go, no CGO dependency).

### Conventions

**Timestamps**: All timestamps are stored as `INTEGER` containing Unix nanoseconds since epoch (UTC). This provides:
- Strong type enforcement — no ambiguous string formats
- Compact storage — 8 bytes per timestamp
- Fast comparisons — integer comparison, no parsing
- Nanosecond precision — avoids the racily-clean problem where file changes within the same second go undetected
- Strict data integrity — `CHECK(col > 0)` constraints reject zero/negative values

Conversion happens at system boundaries only. Internal code uses `int64` nanoseconds exclusively.

**Hashes**: QuickXorHash values are stored as Base64-encoded `TEXT`, matching the Graph API's native format. SHA-256 hashes (Business-only, opportunistic) are stored as hex `TEXT`.

**Identifiers**: UUIDs are stored as `TEXT` (RFC 4122 format). Drive IDs and item IDs are opaque `TEXT` values. All drive IDs are normalized (lowercase, zero-padded) before storage to handle inconsistencies in the Graph API.

---

## 2. Database Lifecycle

### One Database per Drive

Each configured drive gets its own SQLite database file, providing complete isolation between drives. The canonical drive identifier (see [accounts.md](accounts.md) §2) determines the filename, with `:` replaced by `_`:

- **Linux**: `~/.local/share/onedrive-go/state_<type>_<email>[_<site>_<library>].db`
- **macOS**: `~/Library/Application Support/onedrive-go/state_<type>_<email>[_<site>_<library>].db`

Examples:
- `state_personal_toni@outlook.com.db`
- `state_business_alice@contoso.com.db`
- `state_sharepoint_alice@contoso.com_marketing_Documents.db`

See [accounts.md](accounts.md) §3 for the complete file layout.

### Token File Layout

Token files follow the same naming pattern, stored in the same data directory:

- `token_personal_<email>.json`
- `token_business_<email>.json`

SharePoint drives share the business account's token file (same OAuth session). Only the state DB is per-drive. For example, `token_business_alice@contoso.com.json` serves both her OneDrive for Business and all her SharePoint drives.

### Migration Strategy

Schema migrations use the [golang-migrate](https://github.com/golang-migrate/migrate) library:

- **Migration files**: Sequential `.sql` files (e.g., `000001_initial_schema.up.sql`, `000001_initial_schema.down.sql`)
- **Embedded in binary**: Migration files are embedded via Go's `embed.FS` — no external file dependency at runtime
- **Version tracking**: golang-migrate manages its own `schema_migrations` table automatically
- **Up + down**: Every schema change has both up and down migrations for rollback
- **Destructive migrations**: Backup the DB file before running migrations that drop columns or tables

### First Run

On first run with a new drive:

1. Create the database file and parent directories
2. Set SQLite pragmas (WAL, FULL synchronous, foreign keys)
3. Run all pending migrations via golang-migrate to create the schema
4. The database is ready for use

### Crash Recovery

SQLite WAL mode ensures the database is always in a consistent state after a crash. On restart, the sync engine re-fetches from the last saved delta token. At most one batch of items (default 500) may need reprocessing.

---

## 3. Items Table

The items table is the core of the sync database. Every synced file, folder, root, and remote item reference is a row. It implements the three-state model that is universal best practice among file sync tools.

```sql
CREATE TABLE items (
    -- Identity (architecture §6.3)
    drive_id        TEXT    NOT NULL,  -- normalized: lowercase, zero-padded for Personal
    item_id         TEXT    NOT NULL,  -- opaque server ID
    parent_drive_id TEXT,              -- parent's drive (for cross-drive references)
    parent_id       TEXT,              -- parent item ID
    name            TEXT    NOT NULL,  -- display name (Business deleted items: looked up from DB)
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root', 'remote')),
    path            TEXT,              -- materialized local path (architecture §6.4)

    -- Remote state (from Graph API, updated by delta processor)
    size            INTEGER,           -- bytes (nullable: deleted Personal items lack size)
    etag            TEXT,              -- entity tag (nullable: Business/SP root items lack eTag)
    ctag            TEXT,              -- content tag (nullable: Business folders, Business delta)
    quick_xor_hash  TEXT,              -- Base64-encoded QuickXorHash (files only, bogus on deleted)
    sha256_hash     TEXT,              -- hex SHA-256 (Business-only, opportunistic)
    remote_mtime    INTEGER,           -- server lastModifiedDateTime as Unix nanoseconds

    -- Local state (from filesystem scanner)
    local_size      INTEGER,           -- last-known local file size in bytes
    local_mtime     INTEGER,           -- local modification time as Unix nanoseconds
    local_hash      TEXT,              -- last-computed local QuickXorHash (Base64)

    -- Sync base state (snapshot at last successful sync)
    synced_size     INTEGER,           -- size at last successful sync
    synced_mtime    INTEGER,           -- mtime at last successful sync (Unix nanoseconds)
    synced_hash     TEXT,              -- hash at last successful sync (Base64)
    last_synced_at  INTEGER,           -- timestamp of last sync operation (Unix nanoseconds)

    -- Shared/remote item references (architecture §6 remote items)
    remote_drive_id TEXT,              -- target drive for shared/remote items
    remote_id       TEXT,              -- target item ID for shared/remote items

    -- Tombstone fields (architecture §6.6)
    is_deleted      INTEGER NOT NULL DEFAULT 0 CHECK(is_deleted IN (0, 1)),
    deleted_at      INTEGER,           -- tombstone creation timestamp (Unix nanoseconds)

    -- Row metadata
    created_at      INTEGER NOT NULL CHECK(created_at > 0),  -- row creation (Unix nanoseconds)
    updated_at      INTEGER NOT NULL CHECK(updated_at > 0),  -- row last update (Unix nanoseconds)

    PRIMARY KEY (drive_id, item_id)
);
```

### Column Notes

| Column | Nullable? | Why |
|--------|-----------|-----|
| `size` | Yes | Personal deleted items omit size |
| `etag` | Yes | Business/SharePoint root items omit eTag |
| `ctag` | Yes | Business folders never have cTag; Business delta create/modify omits cTag |
| `quick_xor_hash` | Yes | Only present for files; bogus on deleted items |
| `sha256_hash` | Yes | Business-only, officially unsupported, sometimes present |
| `remote_mtime` | Yes | May be absent for API-initiated deletions |
| `local_*` | Yes | Not populated until local scanner has run |
| `synced_*` | Yes | Not populated until first successful sync of this item |
| `remote_drive_id`, `remote_id` | Yes | Only populated for `item_type = 'remote'` |
| `deleted_at` | Yes | Only set when `is_deleted = 1` |
| `path` | Yes | Root items have no meaningful path; newly discovered items may not have computed paths yet |

### Three-State Model

The items table encodes three views of each item in a single row:

| State | Columns | Source |
|-------|---------|--------|
| **Remote (current)** | `quick_xor_hash`, `size`, `remote_mtime`, `etag`, `ctag` | Graph API delta responses |
| **Local (current)** | `local_hash`, `local_size`, `local_mtime` | Filesystem scanner |
| **Synced (base)** | `synced_hash`, `synced_size`, `synced_mtime`, `last_synced_at` | Snapshot after last successful sync |

This layout follows the three-tree model used by production-grade sync systems like Dropbox Nucleus.

---

## 4. Delta Tokens Table

Stores the Graph API delta query cursor per drive. This is a first-class piece of sync state that must be persisted across restarts.

```sql
CREATE TABLE delta_tokens (
    drive_id   TEXT    PRIMARY KEY,
    token      TEXT    NOT NULL,      -- opaque delta token from Graph API
    updated_at INTEGER NOT NULL CHECK(updated_at > 0)  -- last update (Unix nanoseconds)
);
```

On HTTP 410 (token expired), the sync engine deletes the token and falls back to full enumeration.

---

## 5. Conflict Ledger

Per-file conflict tracking as specified in architecture §6.7. The `history` column stores a JSON array of resolution events, keeping a full audit trail without requiring an additional table.

```sql
CREATE TABLE conflicts (
    id            TEXT    PRIMARY KEY,  -- UUID (RFC 4122)
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL,     -- file path at time of conflict detection
    detected_at   INTEGER NOT NULL CHECK(detected_at > 0),  -- Unix nanoseconds
    local_hash    TEXT,                 -- QuickXorHash of local version (Base64)
    remote_hash   TEXT,                 -- QuickXorHash of remote version (Base64)
    local_mtime   INTEGER,             -- local mtime at conflict (Unix nanoseconds)
    remote_mtime  INTEGER,             -- remote mtime at conflict (Unix nanoseconds)
    resolution    TEXT    NOT NULL DEFAULT 'unresolved'
                         CHECK(resolution IN (
                             'unresolved',
                             'keep_both',
                             'keep_local',
                             'keep_remote',
                             'manual'
                         )),
    resolved_at   INTEGER,             -- resolution timestamp (Unix nanoseconds)
    resolved_by   TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history       TEXT,                 -- JSON array: [{"action":"...","at":123,"by":"..."},...]

    FOREIGN KEY (drive_id, item_id) REFERENCES items(drive_id, item_id)
);
```

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
    {"action": "keep_both", "at": 1708123460000000000, "by": "auto", "renamed": "file.conflict.20240217T120000.txt"},
    {"action": "keep_local", "at": 1708200000000000000, "by": "user"}
]
```

---

## 6. Stale Files Ledger

Tracks files that became excluded by filter changes but still exist locally (architecture §6.8). The sync engine **never auto-deletes** stale files — users must explicitly dispose of each.

```sql
CREATE TABLE stale_files (
    id          TEXT    PRIMARY KEY,   -- UUID (RFC 4122)
    path        TEXT    NOT NULL,      -- local file path
    reason      TEXT    NOT NULL,      -- why it became stale (e.g., "excluded by skip_files pattern *.tmp")
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),  -- Unix nanoseconds
    size        INTEGER                -- file size in bytes (for display)
);
```

---

## 7. Upload Sessions

Tracks resumable upload sessions for large files. The `session_url` is pre-authenticated (no Bearer token needed). Fragments must be 320 KiB multiples.

```sql
CREATE TABLE upload_sessions (
    id             TEXT    PRIMARY KEY, -- UUID (RFC 4122)
    drive_id       TEXT    NOT NULL,
    item_id        TEXT,               -- null for new file uploads (item ID assigned after completion)
    local_path     TEXT    NOT NULL,    -- source file on local filesystem
    session_url    TEXT    NOT NULL,    -- pre-authenticated upload URL from Graph API
    expiry         INTEGER NOT NULL CHECK(expiry > 0),     -- session expiration (Unix nanoseconds)
    bytes_uploaded INTEGER NOT NULL DEFAULT 0 CHECK(bytes_uploaded >= 0),
    total_size     INTEGER NOT NULL CHECK(total_size > 0), -- total file size in bytes
    created_at     INTEGER NOT NULL CHECK(created_at > 0)  -- Unix nanoseconds
);
```

On startup, expired sessions (`expiry < now`) are cleaned up. Active sessions are resumed from `bytes_uploaded`.

---

## 8. Config Snapshot

Detects configuration changes that require stale file tracking. When filter patterns, sync paths, or other filter-affecting config changes, the sync engine compares current values against this snapshot to identify newly-stale files.

```sql
CREATE TABLE config_snapshot (
    key   TEXT PRIMARY KEY,  -- config key (e.g., "skip_files", "sync_paths")
    value TEXT NOT NULL       -- serialized config value
);
```

---

## 9. Indexes and Performance

### Primary Indexes

```sql
-- Path lookups: find item by local path
CREATE INDEX idx_items_path ON items(path);

-- Tree traversal: list children of a folder
CREATE INDEX idx_items_parent ON items(parent_drive_id, parent_id);

-- Tombstone cleanup: efficiently find expired tombstones
CREATE INDEX idx_items_tombstone ON items(is_deleted, deleted_at)
    WHERE is_deleted = 1;

-- Type-based queries: list all files, all folders, etc.
CREATE INDEX idx_items_type ON items(item_type);

-- Conflict lookup by item: find conflicts for a specific item
CREATE INDEX idx_conflicts_item ON conflicts(drive_id, item_id);
```

### Performance Guidelines

**WAL checkpointing**: After every batch of items processed (default 500, configurable), perform a WAL checkpoint to bound WAL file growth. This matches architecture §6.5.

**VACUUM**: Run only on schema migrations, not as routine maintenance. Routine VACUUM is expensive and provides minimal benefit when the database is under steady-state use.

**Prepared statements**: All repeated queries use prepared statements cached for the lifetime of the database connection. This avoids re-parsing SQL on every call.

**Covering indexes**: The `idx_items_path` and `idx_items_parent` indexes cover the most frequent query patterns:
- `GetItemByPath`: looks up by `path`
- `ListChildren`: looks up by `(parent_drive_id, parent_id)`
- `CleanupTombstones`: scans by `(is_deleted, deleted_at)` using partial index

**Batch operations**: Delta processing inserts/updates items in batches within a single transaction, minimizing fsync overhead.

---

## 10. Path Materialization

Materialized paths avoid the expensive O(depth) parent-chain walk on every access that a naive approach would require (architecture §6.4).

### Algorithm: Compute Path

On item insert or update when the parent changes:

```
function MaterializePath(driveID, itemID):
    item = GetItem(driveID, itemID)
    if item.type == "root":
        return "/"

    segments = []
    current = item
    while current.parentID != nil:
        segments = prepend(current.name, segments)
        current = GetItem(current.parentDriveID, current.parentID)

    return "/" + join(segments, "/")
```

### Algorithm: Cascade on Rename/Move

When a folder is renamed or moved, all descendants' paths must be updated:

```
function CascadePathUpdate(oldPrefix, newPrefix):
    UPDATE items
    SET path = newPrefix || substr(path, length(oldPrefix) + 1),
        updated_at = NowNano()
    WHERE path LIKE oldPrefix || '/%'
       OR path = oldPrefix
```

This uses SQLite's string functions for an efficient single-statement bulk update. The `LIKE` prefix match leverages the `idx_items_path` index.

### When Paths Are Recomputed

| Event | Action |
|-------|--------|
| New item from delta | Compute path from parent chain, store in `path` |
| Item rename (same parent) | Update own `path`; cascade to descendants |
| Item move (new parent) | Recompute own `path` from new parent chain; cascade to descendants |
| Parent rename/move | Cascading update from parent's path change |
| Full resync | Recompute all paths from scratch |

---

## 11. Tombstone Lifecycle

Tombstones enable move detection across sync cycles: when an item ID disappears from one parent and reappears under another, the tombstone allows the sync engine to recognize this as a move rather than a delete + create (architecture §6.6).

### Marking as Deleted

Instead of `DELETE FROM items`, set the tombstone flags:

```sql
UPDATE items
SET is_deleted = 1,
    deleted_at = :now_ns,
    updated_at = :now_ns
WHERE drive_id = :drive_id AND item_id = :item_id;
```

The item retains its `name`, `path`, `drive_id`, and `item_id` — all needed for move detection and conflict resolution.

### Move Detection

When processing delta results, if an item ID that was previously marked as deleted reappears:

1. The item's `is_deleted` flag is cleared
2. The new parent and name are applied
3. The path is recomputed
4. This is logged as a move, not a new creation

### Cleanup

Expired tombstones are purged periodically:

```sql
DELETE FROM items
WHERE is_deleted = 1
  AND deleted_at < :cutoff_ns;
```

Where `cutoff_ns = NowNano() - (retention_days * 86400 * 1_000_000_000)`.

**Default retention**: 30 days (configurable via `tombstone_retention_days` in config).

### Cleanup Scheduling

Tombstone cleanup runs:
- At the start of each sync cycle (before delta processing)
- Only for items older than the retention period
- Within the same transaction as the delta processing batch

---

## 12. Three-Way Merge Data Flow

The reconciler reads the three states from each item row and determines what action to take. This section maps item table columns to the merge algorithm.

### Change Detection

```
Local changed?  = (local_hash  != synced_hash) OR (local_size  != synced_size)
Remote changed? = (quick_xor_hash != synced_hash) OR (remote_mtime != synced_mtime)
```

### Decision Matrix

| Local Changed? | Remote Changed? | Hashes Equal? | Action |
|:-:|:-:|:-:|--------|
| No | No | - | No action (in sync) |
| No | Yes | - | Pull: download remote to local |
| Yes | No | - | Push: upload local to remote |
| Yes | Yes | Yes | False conflict: both sides converged. Update synced state. |
| Yes | Yes | No | **Conflict**: record in conflict ledger, apply resolution policy |

### False Conflict Detection

A "false conflict" occurs when both sides changed independently but arrived at the same content:

```
Both changed AND (local_hash == quick_xor_hash)
```

This is the same pattern used by rclone bisync and Unison. False conflicts are resolved silently by updating the synced state to match both sides.

### After Successful Sync

After a file is successfully synced (upload verified, download hash-checked), the synced state is updated:

```sql
UPDATE items
SET synced_hash    = :current_hash,
    synced_size    = :current_size,
    synced_mtime   = :current_mtime,
    last_synced_at = :now_ns,
    updated_at     = :now_ns
WHERE drive_id = :drive_id AND item_id = :item_id;
```

---

## 13. Timestamp Helpers (Go Layer)

All timestamp conversion happens at system boundaries. Internal code uses `int64` nanoseconds exclusively.

### Functions

| Function | Signature | Description |
|----------|-----------|-------------|
| `NowNano` | `func NowNano() int64` | Returns `time.Now().UnixNano()` |
| `ParseAPITime` | `func ParseAPITime(s string) int64` | Parses ISO 8601 from Graph API to nanoseconds. Validates range; returns `NowNano()` for invalid/missing timestamps. |
| `FormatAPITime` | `func FormatAPITime(ns int64) string` | Nanoseconds to ISO 8601 for API requests |
| `FormatHuman` | `func FormatHuman(ns int64) string` | Nanoseconds to human-readable display (e.g., `2024-02-17 12:00:00 UTC`) |
| `ToUnixNano` | `func ToUnixNano(t time.Time) int64` | Converts `time.Time` to nanoseconds (for local file stat) |

### Validation Rules

- **Invalid dates**: `0001-01-01T00:00:00Z` or dates before 1970 -> fall back to `NowNano()`
- **Far-future dates**: Dates more than 1 year in the future -> fall back to `NowNano()`
- **Fractional seconds**: OneDrive does not store fractional seconds; truncate to whole seconds before comparison (but store full nanosecond precision from local filesystem)
- **Timezone**: All API timestamps are UTC. All stored timestamps are UTC nanoseconds.

---

## Appendix A: Architecture Constraint Traceability

| Architecture Constraint | Section | Data Model Implementation |
|------------------------|---------|--------------------------|
| modernc.org/sqlite, WAL, FULL synchronous | §6.1 | §1 SQLite Configuration |
| Separate DB per drive | §6.2 | §2 Database Lifecycle |
| Primary key: (driveId, itemId) composite | §6.3 | §3 Items Table — `PRIMARY KEY (drive_id, item_id)` |
| Materialized paths, rebuilt on parent change | §6.4 | §10 Path Materialization |
| Checkpoints: configurable batch, delta tokens per drive | §6.5 | §4 Delta Tokens Table, §9 WAL checkpointing |
| Tombstones: 30-day retention, configurable | §6.6 | §11 Tombstone Lifecycle |
| Conflict ledger | §6.7 | §5 Conflict Ledger |
| Stale files ledger | §6.8 | §6 Stale Files Ledger |
| Single writer goroutine, concurrent readers | §5.1 | §1 WAL mode enables this pattern |
| Three-way merge support | §4.1 step 6 | §3 Three-State Model, §12 Three-Way Merge Data Flow |

## Appendix B: Full DDL Summary

For reference, the complete DDL in execution order:

```sql
-- Pragmas
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA journal_size_limit = 67108864;

-- Tables
CREATE TABLE items (
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_drive_id TEXT,
    parent_id       TEXT,
    name            TEXT    NOT NULL,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root', 'remote')),
    path            TEXT,
    size            INTEGER,
    etag            TEXT,
    ctag            TEXT,
    quick_xor_hash  TEXT,
    sha256_hash     TEXT,
    remote_mtime    INTEGER,
    local_size      INTEGER,
    local_mtime     INTEGER,
    local_hash      TEXT,
    synced_size     INTEGER,
    synced_mtime    INTEGER,
    synced_hash     TEXT,
    last_synced_at  INTEGER,
    remote_drive_id TEXT,
    remote_id       TEXT,
    is_deleted      INTEGER NOT NULL DEFAULT 0 CHECK(is_deleted IN (0, 1)),
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL CHECK(created_at > 0),
    updated_at      INTEGER NOT NULL CHECK(updated_at > 0),
    PRIMARY KEY (drive_id, item_id)
);

CREATE TABLE delta_tokens (
    drive_id   TEXT    PRIMARY KEY,
    token      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL CHECK(updated_at > 0)
);

CREATE TABLE conflicts (
    id            TEXT    PRIMARY KEY,
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL,
    detected_at   INTEGER NOT NULL CHECK(detected_at > 0),
    local_hash    TEXT,
    remote_hash   TEXT,
    local_mtime   INTEGER,
    remote_mtime  INTEGER,
    resolution    TEXT    NOT NULL DEFAULT 'unresolved'
                         CHECK(resolution IN (
                             'unresolved', 'keep_both', 'keep_local',
                             'keep_remote', 'manual'
                         )),
    resolved_at   INTEGER,
    resolved_by   TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history       TEXT,
    FOREIGN KEY (drive_id, item_id) REFERENCES items(drive_id, item_id)
);

CREATE TABLE stale_files (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL,
    reason      TEXT    NOT NULL,
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),
    size        INTEGER
);

CREATE TABLE upload_sessions (
    id             TEXT    PRIMARY KEY,
    drive_id       TEXT    NOT NULL,
    item_id        TEXT,
    local_path     TEXT    NOT NULL,
    session_url    TEXT    NOT NULL,
    expiry         INTEGER NOT NULL CHECK(expiry > 0),
    bytes_uploaded INTEGER NOT NULL DEFAULT 0 CHECK(bytes_uploaded >= 0),
    total_size     INTEGER NOT NULL CHECK(total_size > 0),
    created_at     INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TABLE config_snapshot (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Indexes
CREATE INDEX idx_items_path ON items(path);
CREATE INDEX idx_items_parent ON items(parent_drive_id, parent_id);
CREATE INDEX idx_items_tombstone ON items(is_deleted, deleted_at) WHERE is_deleted = 1;
CREATE INDEX idx_items_type ON items(item_type);
CREATE INDEX idx_conflicts_item ON conflicts(drive_id, item_id);
```
