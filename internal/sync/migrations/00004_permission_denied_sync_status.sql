-- +goose Up
-- Expand local_issues sync_status CHECK to include permission_denied.
-- SQLite requires table rebuild to alter CHECK constraints.

CREATE TABLE local_issues_new (
    path          TEXT    PRIMARY KEY,
    issue_type    TEXT    NOT NULL
                  CHECK(issue_type IN (
                      'invalid_filename', 'path_too_long', 'file_too_large',
                      'permission_denied', 'upload_failed', 'quota_exceeded',
                      'locked', 'sharepoint_restriction')),
    sync_status   TEXT    NOT NULL DEFAULT 'pending_upload'
                  CHECK(sync_status IN (
                      'pending_upload', 'uploading', 'upload_failed',
                      'permanently_failed', 'resolved', 'permission_denied')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    file_size     INTEGER,
    local_hash    TEXT
);

INSERT INTO local_issues_new SELECT * FROM local_issues;
DROP TABLE local_issues;
ALTER TABLE local_issues_new RENAME TO local_issues;

-- +goose Down
-- Revert: remove permission_denied from sync_status CHECK.

CREATE TABLE local_issues_old (
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

-- Delete permission_denied rows before migrating back (they can't fit the old constraint).
DELETE FROM local_issues WHERE sync_status = 'permission_denied';
INSERT INTO local_issues_old SELECT * FROM local_issues;
DROP TABLE local_issues;
ALTER TABLE local_issues_old RENAME TO local_issues;
