-- +goose Up

-- Unified failure tracking: replaces local_issues and remote_state failure columns.
-- All failure metadata (download, upload, delete) lives in one table.
-- remote_state retains its sync_status state machine but failure metadata
-- (failure_count, next_retry_at, last_error, http_status) moves here.
CREATE TABLE IF NOT EXISTS sync_failures (
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

CREATE INDEX IF NOT EXISTS idx_sync_failures_retry ON sync_failures(next_retry_at)
    WHERE next_retry_at IS NOT NULL AND category = 'transient';

-- Data migration: copy existing local_issues rows.
-- drive_id defaults to '' (populated by engine on next failure for that path).
INSERT OR IGNORE INTO sync_failures (path, drive_id, direction, category, issue_type,
    failure_count, next_retry_at, last_error, http_status,
    first_seen_at, last_seen_at, file_size, local_hash)
SELECT path, '', 'upload',
    CASE WHEN sync_status = 'permanently_failed' THEN 'permanent' ELSE 'transient' END,
    issue_type, failure_count, next_retry_at, last_error, http_status,
    first_seen_at, last_seen_at, file_size, local_hash
FROM local_issues WHERE sync_status != 'resolved';

-- Data migration: copy remote_state failure rows.
INSERT OR IGNORE INTO sync_failures (path, drive_id, direction, category, item_id,
    failure_count, next_retry_at, last_error, http_status,
    first_seen_at, last_seen_at)
SELECT path, drive_id,
    CASE WHEN sync_status IN ('download_failed', 'downloading') THEN 'download' ELSE 'delete' END,
    'transient', item_id, failure_count, next_retry_at, last_error, http_status,
    observed_at, observed_at
FROM remote_state WHERE failure_count > 0;

-- Move existing sync_failure conflicts to sync_failures as permanent.
INSERT OR IGNORE INTO sync_failures (path, drive_id, direction, category, item_id,
    failure_count, last_error, first_seen_at, last_seen_at)
SELECT path, drive_id, 'download', 'permanent', item_id,
    10, 'escalated from conflict', detected_at, detected_at
FROM conflicts WHERE conflict_type = 'sync_failure' AND resolution = 'unresolved';

DELETE FROM conflicts WHERE conflict_type = 'sync_failure';

-- Drop legacy table.
DROP TABLE IF EXISTS local_issues;

-- +goose Down
-- Recreate local_issues for rollback.
CREATE TABLE IF NOT EXISTS local_issues (
    path          TEXT    PRIMARY KEY,
    issue_type    TEXT    NOT NULL
                  CHECK(issue_type IN (
                      'invalid_filename', 'path_too_long', 'file_too_large',
                      'permission_denied', 'upload_failed', 'quota_exceeded',
                      'locked', 'sharepoint_restriction')),
    sync_status   TEXT    NOT NULL DEFAULT 'pending_upload'
                  CHECK(sync_status IN (
                      'pending_upload', 'uploading', 'upload_failed',
                      'permanently_failed', 'resolved')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    file_size     INTEGER,
    local_hash    TEXT
);

-- Migrate upload failures back to local_issues.
INSERT OR IGNORE INTO local_issues (path, issue_type, sync_status, failure_count,
    next_retry_at, last_error, http_status, first_seen_at, last_seen_at, file_size, local_hash)
SELECT path,
    COALESCE(issue_type, 'upload_failed'),
    CASE WHEN category = 'permanent' THEN 'permanently_failed' ELSE COALESCE(issue_type, 'upload_failed') END,
    failure_count, next_retry_at, last_error, http_status,
    first_seen_at, last_seen_at, file_size, local_hash
FROM sync_failures WHERE direction = 'upload';

DROP TABLE IF EXISTS sync_failures;
