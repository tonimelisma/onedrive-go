-- +goose Up

-- Consolidated schema for the sync engine state database.
-- Replaces the original 5-migration chain (00001-00005) with a single
-- DDL script. All test databases are created fresh, so no incremental
-- migration path is needed.

-- Core sync state: confirmed synced state per (drive_id, item_id).
-- ID-based PK decouples remote identity from local filesystem paths.
-- Path is a UNIQUE secondary key for fast local lookups.
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

-- Cascading path operations: folder renames update all children by parent_id.
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
    -- history is currently unused/dormant. Reserved for future resolution
    -- audit trail (e.g., JSON array of resolution attempts). Not populated
    -- by any code path; safe to ignore in queries (B-160).
    history         TEXT
);

-- Conflict filtering by resolution status.
CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);

-- Sync metadata key-value store for reporting.
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Full remote mirror: every item the delta API has told us about.
-- ID-based primary key decouples remote identity from local filesystem paths.
-- See docs/design/remote-state-separation.md §5, §7, §15.
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
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    PRIMARY KEY (drive_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_remote_state_status
    ON remote_state(sync_status);
CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
-- Partial unique index: only active (non-deleted) items enforce path uniqueness.
-- Deleted/pending_delete items retain their path for diagnostics but do not
-- block new items at the same path (known delta ordering issue, tier 1 #154).
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_active_path
    ON remote_state(path)
    WHERE sync_status NOT IN ('deleted', 'pending_delete');

-- Local issue tracking for upload failures and path violations.
-- See docs/design/remote-state-separation.md §14, §15.
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

-- +goose Down
DROP TABLE IF EXISTS local_issues;
DROP TABLE IF EXISTS remote_state;
DROP TABLE IF EXISTS sync_metadata;
DROP TABLE IF EXISTS conflicts;
DROP TABLE IF EXISTS delta_tokens;
DROP TABLE IF EXISTS baseline;
