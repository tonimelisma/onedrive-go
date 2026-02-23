# Migration: Batch Pipeline to Event-Driven (Option E)

> **ZERO PATH DEPENDENCY**: This document describes the Option E event-driven
> architecture, designed from first principles. The system is NOT an evolution
> of the prior batch-pipeline sync engine. Existing code is reused only where
> it is an excellent match for the new design. See
> [event-driven-rationale.md](event-driven-rationale.md) for the full rationale.

---

## 1. Overview

The onedrive-go sync engine is being rewritten from a batch-pipeline architecture (Phase 4 v1) to an event-driven architecture (Phase 4 v2, Option E). This document describes what changes during the transition, what is preserved, and how the user experience is affected.

**Why**: Comprehensive E2E analysis of the batch-pipeline engine identified six structural fault lines — all traced to a single root cause: the database being used as the coordination mechanism between pipeline stages. Option E eliminates this root cause entirely. See [event-driven-rationale.md](event-driven-rationale.md) Part 1 for the full analysis.

---

## 2. Code Transition

### 2.1 What Is Deleted

All batch-pipeline sync files in `internal/sync/` that implement the old architecture:

| Old File | Old Responsibility | Replaced By |
|----------|-------------------|-------------|
| `db.go` | SQLite state store (26-column `items` table) | `baseline.go` (13-column `baseline` table) |
| `scan.go` | Local scanner (writes to DB) | `observer_local.go` (produces ChangeEvents) |
| `delta.go` | Delta processor (writes to DB) | `observer_remote.go` (produces ChangeEvents) |
| `reconcile.go` | Three-way reconciler (reads DB) | `planner.go` (pure function on events + baseline) |
| `execute.go` | Action executor (writes to DB) | `executor.go` (produces Outcomes, no DB writes) |
| `conflict.go` | Conflict handler | `conflict.go` (adapted for Outcome model) |
| `safety.go` | Safety checks (queries DB) | Integrated into `planner.go` (pure functions) |
| `transfer.go` | Transfer pipeline | `transfer.go` (adapted to produce Outcomes) |
| `engine.go` | Orchestrator | `engine.go` (rewritten for event-driven pipeline) |
| `types.go` | Shared types (`Item`, `Action`, etc.) | `types.go` (new type system: ChangeEvent, BaselineEntry, PathView, Outcome) |

**Timing**: Old files are deleted in Phase 4v2.0 (Increment 0 — Clean Slate), before new engine implementation begins. The app has no users, so there is no migration path to maintain. The stub sync command returns "not yet implemented" until Increment 8 wires the new engine.

### 2.2 What Is Reused

From [event-driven-rationale.md](event-driven-rationale.md) Part 9:

| Component | Reuse % | Notes |
|-----------|---------|-------|
| `internal/graph/` | **100%** | No changes. All API client code, auth, normalization, types. |
| `internal/config/` | **100%** | No changes. TOML config, drive resolution, validation. |
| `pkg/quickxorhash/` | **100%** | No changes. |
| `cmd/onedrive-go/` | **~90%** | Minor wiring changes in `sync.go` to use new engine API. |
| `e2e/` | **~95%** | E2E tests exercise the CLI, not internal types. Minor adjustments. |
| Filter engine | **~95%** | Same `Filter` interface. Small adapter for `PathView` context. |
| Transfer pipeline | **~80%** | Worker pools, bandwidth limiting, hash verification. Changed to produce Outcomes instead of DB writes. |
| Safety checks | **~90%** | Same S1-S7 invariants. Changed to operate on baseline + plan (pure function). |
| Reconciler logic | **~70%** | Same decision matrix (reorganized as EF1-EF14, ED1-ED8). Different input types. |
| Delta processing | **~60%** | Normalization and conversion reused. Output changes from DB writes to events. |
| Scanner | **~60%** | Walk/hash logic reused. Output changes from DB writes to events. |
| State store (SQLite) | **~30%** | Schema redesign. CRUD rewritten for baseline table. Migration infrastructure reused. |

**Estimated**: ~40% net new code, ~30% rewritten, ~30% deleted.

### 2.3 What Is New

| New Component | File | Purpose |
|---------------|------|---------|
| ChangeEvent type | `types.go` | Immutable observation from remote or local observer |
| BaselineEntry type | `types.go` | Confirmed synced state — the only durable per-item state |
| PathView type | `types.go` | Three-way view (remote + local + baseline) for planner |
| Outcome type | `types.go` | Self-contained result of executing an action |
| Remote Observer | `observer_remote.go` | Delta API → ChangeEvents (no DB writes) |
| Local Observer | `observer_local.go` | FS walk → ChangeEvents (no DB writes) |
| Change Buffer | `buffer.go` | Debounce, dedup, batch events by path |
| Planner | `planner.go` | Pure function: events + baseline → ActionPlan |
| Baseline Manager | `baseline.go` | Sole DB writer. Load, Commit (atomic). |

---

## 3. Schema Migration

### 3.1 Items Table → Baseline Table

The 26-column `items` table is replaced by a 13-column `baseline` table. This is a clean break — no dual-model operation.

**Migration SQL** (applied as schema version 2):

```sql
-- Create new baseline table
CREATE TABLE baseline (
    path            TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_id       TEXT,
    name            TEXT    NOT NULL,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    size            INTEGER,
    mtime           INTEGER,
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
    etag            TEXT,
    ctag            TEXT
);

CREATE UNIQUE INDEX idx_baseline_item ON baseline(drive_id, item_id);
CREATE INDEX idx_baseline_parent ON baseline(parent_id);
CREATE INDEX idx_baseline_path_prefix ON baseline(path);

-- Migrate data from items table (only confirmed synced items)
INSERT INTO baseline (path, drive_id, item_id, parent_id, name, item_type,
                      local_hash, remote_hash, size, mtime, synced_at, etag, ctag)
SELECT
    i.path,
    i.drive_id,
    i.item_id,
    i.parent_id,
    i.name,
    i.item_type,
    COALESCE(i.synced_hash, i.local_hash),   -- local_hash = synced or local
    COALESCE(i.synced_hash, i.remote_hash),  -- remote_hash = synced or remote
    COALESCE(i.local_size, i.remote_size, i.size),
    COALESCE(i.local_mtime, i.remote_mtime),
    COALESCE(i.last_synced_at, i.updated_at, strftime('%s', 'now') * 1000000000),
    i.etag,
    i.ctag
FROM items i
WHERE i.is_deleted = 0
  AND i.synced_hash IS NOT NULL;

-- Drop old table
DROP TABLE IF EXISTS items;
```

### 3.2 Tombstone Handling

Tombstones (rows with `is_deleted = 1`) are **discarded** during migration. They served two purposes in the old architecture:

1. **Move detection across cycles**: In Option E, move detection uses the frozen baseline during observation — tombstones are not needed.
2. **Remote deletion confirmation**: In Option E, remote deletions produce ChangeEvents. The baseline row is removed in the Commit transaction.

### 3.3 Delta Token Preservation

The `delta_tokens` table schema is unchanged. Existing delta tokens are preserved during migration. On the first sync after migration:

1. The delta token from the old engine is used to fetch changes since the last sync
2. The Remote Observer produces ChangeEvents from the delta response
3. The Local Observer produces ChangeEvents from a full scan
4. The Planner classifies paths against the migrated baseline
5. Items that are already in sync (EF1/ED1) produce no actions

### 3.4 Other Tables

| Table | Migration Action |
|-------|-----------------|
| `delta_tokens` | **Preserved** — schema unchanged |
| `conflicts` | **Preserved** — schema unchanged |
| `stale_files` | **Preserved** — schema unchanged |
| `upload_sessions` | **Preserved** — schema unchanged |
| `config_snapshots` | **Preserved** — schema unchanged |
| `schema_migrations` | **Preserved** — version incremented |
| `change_journal` | **Created new** — empty table for optional debugging |

---

## 4. User Impact

### 4.1 What Stays the Same

- **All CLI commands**: `ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `sync`, `login`, `logout`, `whoami`, `status`, `drive add/remove` — unchanged syntax and behavior
- **Config file format**: Same TOML format, same options, same drive sections
- **Config location**: Same XDG paths
- **Token files**: Same location, same format
- **Sync directory**: Same `sync_dir` setting, same file layout
- **Global flags**: `--drive`, `--account`, `--config`, `--json`, `--verbose`, `--debug`, `--quiet`

### 4.2 What Changes

| Area | Change | User Impact |
|------|--------|-------------|
| **First sync after upgrade** | Full delta re-fetch | ~30s delay on first sync (one-time) |
| **`tombstone_retention_days`** | Config option eliminated | Warning logged if present in config; ignored |
| **Dry-run** | Zero side effects (was: wrote to DB) | Better behavior — no hidden state changes |
| **Steady-state memory** | ~20 MB for 100K files (was: ~0 extra) | Baseline cached in memory; offset by fewer DB writes |
| **Sync state DB** | Schema v2 (baseline table) | Automatic migration on first run |

### 4.3 First Sync After Upgrade

On the first sync after migration:

1. Schema migration runs automatically (creates `baseline` table from `items`)
2. Delta token is preserved — only changes since last sync are fetched
3. Full local scan produces events compared against migrated baseline
4. Most items classified as EF1/ED1 (already in sync) — no data transfer
5. Any items that diverged during the upgrade are synced normally

**Worst case**: If the migration cannot convert the `items` table (corrupt or incompatible), the baseline table starts empty and a full re-sync is performed. No data loss — just a longer first sync.

---

## 5. Testing the Migration

### 5.1 Schema Migration Tests

- Verify migration SQL against a populated v1 `items` table
- Verify tombstones are discarded (not migrated)
- Verify `synced_hash` maps correctly to both `local_hash` and `remote_hash`
- Verify items with NULL `synced_hash` are excluded (never synced)
- Verify delta tokens are preserved
- Verify `schema_migrations` version is incremented

### 5.2 Integration Tests

- Run old engine, populate items table, run migration, run new engine
- Verify steady-state: no spurious downloads/uploads after migration
- Verify delta continuation: new changes after migration are detected correctly

### 5.3 E2E Tests

- Existing E2E tests exercise the CLI → engine boundary
- After migration, all existing E2E tests must pass without modification
- New E2E tests for Option E-specific behavior (zero dry-run side effects, etc.)
