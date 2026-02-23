-- +goose Up

-- Core sync state: confirmed synced state per path.
-- 11 columns. Primary key on path for local operations.
CREATE TABLE IF NOT EXISTS baseline (
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

-- Graph API delta cursor per drive.
CREATE TABLE IF NOT EXISTS delta_tokens (
    drive_id    TEXT    PRIMARY KEY,
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0)
);

-- Conflict ledger with resolution history.
CREATE TABLE IF NOT EXISTS conflicts (
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

-- Files excluded by filter changes.
CREATE TABLE IF NOT EXISTS stale_files (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL UNIQUE,
    reason      TEXT    NOT NULL,
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),
    size        INTEGER
);

-- Resumable upload sessions for crash recovery.
CREATE TABLE IF NOT EXISTS upload_sessions (
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

-- Debugging audit trail (optional, append-only).
CREATE TABLE IF NOT EXISTS change_journal (
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

-- Filter change detection snapshots.
CREATE TABLE IF NOT EXISTS config_snapshots (
    key         TEXT    PRIMARY KEY,
    value       TEXT    NOT NULL
);

-- Move detection: look up baseline entry by server-assigned item_id.
CREATE UNIQUE INDEX IF NOT EXISTS idx_baseline_item ON baseline(drive_id, item_id);

-- Cascading path operations: folder renames update all children by parent_id.
CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id);

-- Conflict filtering by resolution status.
CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);

-- Change journal queries by time range.
CREATE INDEX IF NOT EXISTS idx_journal_timestamp ON change_journal(timestamp);
