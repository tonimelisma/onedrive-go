-- +goose Up
-- Add 'sync_failure' to conflicts.conflict_type CHECK constraint.
-- SQLite can't ALTER CHECK constraints, so recreate the table.

CREATE TABLE conflicts_new (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    path            TEXT    NOT NULL,
    conflict_type   TEXT    NOT NULL CHECK(conflict_type IN (
                                'edit_edit', 'edit_delete', 'create_create', 'sync_failure'
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

INSERT INTO conflicts_new SELECT * FROM conflicts;
DROP TABLE conflicts;
ALTER TABLE conflicts_new RENAME TO conflicts;

CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);

-- +goose Down
-- Reverse: recreate without 'sync_failure'.
CREATE TABLE conflicts_old (
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

INSERT INTO conflicts_old SELECT * FROM conflicts WHERE conflict_type != 'sync_failure';
DROP TABLE conflicts;
ALTER TABLE conflicts_old RENAME TO conflicts;

CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);
