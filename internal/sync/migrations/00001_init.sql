-- +goose Up
-- Initial sync-store schema. The database is opened only through goose-backed
-- migrations so the durable state model stays explicit and rebuildable.

-- Core sync state: confirmed synced state per (drive_id, item_id).
CREATE TABLE IF NOT EXISTS baseline (
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    path            TEXT    NOT NULL UNIQUE,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    local_size      INTEGER,
    remote_size     INTEGER,
    local_mtime     INTEGER,
    remote_mtime    INTEGER,
    synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
    etag            TEXT,
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id);

-- Graph API delta cursor per drive with composite key for scoped-root support.
CREATE TABLE IF NOT EXISTS delta_tokens (
    drive_id    TEXT    NOT NULL,
    scope_id    TEXT    NOT NULL DEFAULT '',
    scope_drive TEXT    NOT NULL DEFAULT '',
    cursor      TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0),
    PRIMARY KEY (drive_id, scope_id)
);

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
    observed_at   INTEGER NOT NULL CHECK(observed_at > 0),
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_path
    ON remote_state(path);

-- Unified per-item failure tracking.
--
-- Explicit failure_role replaces implicit interpretation via category,
-- scope_key, and next_retry_at:
--   - item: ordinary per-path failure or actionable issue
--   - held: path currently blocked behind an active scope
--   - boundary: actionable row that defines a scope-backed condition
CREATE TABLE IF NOT EXISTS sync_failures (
    path           TEXT    NOT NULL,
    drive_id       TEXT    NOT NULL,
    direction      TEXT    NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
    action_type    TEXT    NOT NULL CHECK(action_type IN (
                    'download', 'upload', 'local_delete', 'remote_delete',
                    'local_move', 'remote_move', 'folder_create', 'conflict',
                    'update_synced', 'cleanup')),
    category       TEXT    NOT NULL CHECK(category IN ('transient', 'actionable')),
    failure_role   TEXT    NOT NULL CHECK(failure_role IN ('item', 'held', 'boundary')),
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
    scope_key      TEXT    NOT NULL DEFAULT '',
    CHECK (
        (action_type = 'upload' AND direction = 'upload')
        OR (action_type IN ('local_delete', 'remote_delete') AND direction = 'delete')
        OR (action_type IN (
            'download', 'folder_create', 'local_move', 'remote_move',
            'conflict', 'update_synced', 'cleanup'
        ) AND direction = 'download')
    ),
    CHECK (
        failure_role = 'item'
        OR (failure_role = 'held'
            AND category = 'transient'
            AND scope_key <> ''
            AND next_retry_at IS NULL)
        OR (failure_role = 'boundary'
            AND category = 'actionable'
            AND scope_key <> ''
            AND next_retry_at IS NULL)
    ),
    PRIMARY KEY (path, drive_id)
);

CREATE INDEX IF NOT EXISTS idx_sync_failures_retry ON sync_failures(next_retry_at)
    WHERE next_retry_at IS NOT NULL AND category = 'transient';
CREATE INDEX IF NOT EXISTS idx_sync_failures_scope_role
    ON sync_failures(scope_key, failure_role);
CREATE INDEX IF NOT EXISTS idx_sync_failures_remote_blocked
    ON sync_failures(scope_key, last_seen_at DESC)
    WHERE failure_role = 'held' AND scope_key LIKE 'perm:remote:%';
CREATE UNIQUE INDEX IF NOT EXISTS idx_sync_failures_boundary_scope
    ON sync_failures(scope_key)
    WHERE failure_role = 'boundary';

-- Persisted scope-level blocking conditions.
--
-- Runtime ownership lives in the watch event loop. This table is only the
-- crash-safe record used for startup repair and recovery.
CREATE TABLE IF NOT EXISTS scope_blocks (
    scope_key      TEXT PRIMARY KEY,
    issue_type     TEXT NOT NULL,
    timing_source  TEXT NOT NULL CHECK(timing_source IN ('none', 'backoff', 'server_retry_after')),
    blocked_at     INTEGER NOT NULL,
    trial_interval INTEGER NOT NULL,
    next_trial_at  INTEGER NOT NULL,
    preserve_until INTEGER NOT NULL DEFAULT 0,
    trial_count    INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS scope_blocks;
DROP TABLE IF EXISTS sync_failures;
DROP TABLE IF EXISTS remote_state;
DROP TABLE IF EXISTS sync_metadata;
DROP TABLE IF EXISTS delta_tokens;
DROP TABLE IF EXISTS baseline;
