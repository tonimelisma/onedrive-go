-- +goose Up

-- Consolidated schema for the sync engine state database.
-- Single DDL script — no incremental migration chain needed.

-- Core sync state: confirmed synced state per (drive_id, item_id).
CREATE TABLE IF NOT EXISTS baseline (
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    path            TEXT    NOT NULL UNIQUE,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    size            INTEGER,
    mtime           INTEGER,
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
    etag            TEXT,
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id);

-- Graph API delta cursor per drive with composite key for shared folder support.
CREATE TABLE IF NOT EXISTS delta_tokens (
    drive_id    TEXT    NOT NULL,
    scope_id    TEXT    NOT NULL DEFAULT '',
    scope_drive TEXT    NOT NULL DEFAULT '',
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0),
    PRIMARY KEY (drive_id, scope_id)
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

CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);

-- Sync metadata key-value store for reporting.
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Full remote mirror: every item the delta API has told us about.
CREATE TABLE IF NOT EXISTS remote_state (
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

CREATE INDEX IF NOT EXISTS idx_remote_state_status
    ON remote_state(sync_status);
CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_active_path
    ON remote_state(path)
    WHERE sync_status NOT IN ('deleted', 'pending_delete');

-- Unified failure tracking for all sync failure types (download, upload, delete).
CREATE TABLE IF NOT EXISTS sync_failures (
    path           TEXT    NOT NULL,
    drive_id       TEXT    NOT NULL,
    direction      TEXT    NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
    category       TEXT    NOT NULL CHECK(category IN ('transient', 'actionable')),
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

CREATE INDEX IF NOT EXISTS idx_sync_failures_retry ON sync_failures(next_retry_at)
    WHERE next_retry_at IS NOT NULL AND category = 'transient';

-- Shortcut registry for shared folder sync.
CREATE TABLE IF NOT EXISTS shortcuts (
    item_id        TEXT    PRIMARY KEY,
    remote_drive   TEXT    NOT NULL,
    remote_item    TEXT    NOT NULL,
    local_path     TEXT    NOT NULL,
    drive_type     TEXT    NOT NULL DEFAULT '',
    observation    TEXT    NOT NULL DEFAULT 'unknown'
                   CHECK(observation IN ('unknown', 'delta', 'enumerate')),
    read_only      INTEGER NOT NULL DEFAULT 0,
    discovered_at  INTEGER NOT NULL CHECK(discovered_at > 0)
);

-- +goose Down
DROP TABLE IF EXISTS shortcuts;
DROP TABLE IF EXISTS sync_failures;
DROP TABLE IF EXISTS remote_state;
DROP TABLE IF EXISTS sync_metadata;
DROP TABLE IF EXISTS conflicts;
DROP TABLE IF EXISTS delta_tokens;
DROP TABLE IF EXISTS baseline;
