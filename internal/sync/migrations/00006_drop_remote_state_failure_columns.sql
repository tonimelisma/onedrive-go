-- +goose Up

-- Drop dead failure columns from remote_state. These columns were superseded
-- by the sync_failures table (migration 00005) and are no longer read or
-- written. SQLite requires a table rebuild to remove columns (B-336).

CREATE TABLE remote_state_new (
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

INSERT INTO remote_state_new (drive_id, item_id, path, parent_id, item_type,
    hash, size, mtime, etag, previous_path, sync_status, observed_at)
SELECT drive_id, item_id, path, parent_id, item_type,
    hash, size, mtime, etag, previous_path, sync_status, observed_at
FROM remote_state;

DROP TABLE remote_state;

ALTER TABLE remote_state_new RENAME TO remote_state;

CREATE INDEX IF NOT EXISTS idx_remote_state_status
    ON remote_state(sync_status);
CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_active_path
    ON remote_state(path)
    WHERE sync_status NOT IN ('deleted', 'pending_delete');

-- +goose Down

-- Restore the four failure columns by rebuilding the table.
CREATE TABLE remote_state_old (
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

INSERT INTO remote_state_old (drive_id, item_id, path, parent_id, item_type,
    hash, size, mtime, etag, previous_path, sync_status, observed_at)
SELECT drive_id, item_id, path, parent_id, item_type,
    hash, size, mtime, etag, previous_path, sync_status, observed_at
FROM remote_state;

DROP TABLE remote_state;

ALTER TABLE remote_state_old RENAME TO remote_state;

CREATE INDEX IF NOT EXISTS idx_remote_state_status
    ON remote_state(sync_status);
CREATE INDEX IF NOT EXISTS idx_remote_state_parent
    ON remote_state(parent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_active_path
    ON remote_state(path)
    WHERE sync_status NOT IN ('deleted', 'pending_delete');
