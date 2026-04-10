-- +goose Up
-- Split conflict request workflow from derived conflict facts so the durable
-- user-intent lifecycle has its own authoritative table.

ALTER TABLE conflicts RENAME TO conflicts_legacy;

CREATE TABLE conflicts (
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
                                'keep_remote'
                            )),
    resolved_at     INTEGER,
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL)
);

CREATE TABLE conflict_requests (
    conflict_id          TEXT    PRIMARY KEY REFERENCES conflicts(id) ON DELETE CASCADE,
    requested_resolution TEXT    NOT NULL CHECK(requested_resolution IN (
                               'keep_both', 'keep_local', 'keep_remote'
                           )),
    state                TEXT    NOT NULL CHECK(state IN (
                               'resolution_requested', 'resolving', 'resolve_failed'
                           )),
    requested_at         INTEGER,
    resolving_at         INTEGER,
    resolution_error     TEXT
);

INSERT INTO conflicts (
    id, drive_id, item_id, path, conflict_type, detected_at,
    local_hash, remote_hash, local_mtime, remote_mtime,
    resolution, resolved_at, resolved_by
)
SELECT
    id, drive_id, item_id, path, conflict_type, detected_at,
    local_hash, remote_hash, local_mtime, remote_mtime,
    resolution, resolved_at, resolved_by
FROM conflicts_legacy;

INSERT INTO conflict_requests (
    conflict_id,
    requested_resolution,
    state,
    requested_at,
    resolving_at,
    resolution_error
)
SELECT
    id,
    requested_resolution,
    state,
    requested_at,
    resolving_at,
    resolution_error
FROM conflicts_legacy
WHERE state IN ('resolution_requested', 'resolving', 'resolve_failed');

DROP TABLE conflicts_legacy;

CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);
CREATE INDEX idx_conflict_requests_state ON conflict_requests(state);

-- +goose Down
-- Reconstruct the legacy mixed-authority conflicts table from split facts and
-- request workflow rows, then drop the dedicated request table.

ALTER TABLE conflicts RENAME TO conflicts_facts;

CREATE TABLE conflicts (
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
                                'keep_remote'
                            )),
    state           TEXT    NOT NULL DEFAULT 'unresolved'
                            CHECK(state IN (
                                'unresolved', 'resolution_requested', 'resolving',
                                'resolve_failed', 'resolved'
                            )),
    requested_resolution TEXT CHECK(requested_resolution IN (
                                'keep_both', 'keep_local', 'keep_remote'
                            ) OR requested_resolution IS NULL),
    requested_at    INTEGER,
    resolving_at    INTEGER,
    resolution_error TEXT,
    resolved_at     INTEGER,
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history         TEXT
);

INSERT INTO conflicts (
    id, drive_id, item_id, path, conflict_type, detected_at,
    local_hash, remote_hash, local_mtime, remote_mtime,
    resolution, state, requested_resolution, requested_at,
    resolving_at, resolution_error, resolved_at, resolved_by
)
SELECT
    c.id,
    c.drive_id,
    c.item_id,
    c.path,
    c.conflict_type,
    c.detected_at,
    c.local_hash,
    c.remote_hash,
    c.local_mtime,
    c.remote_mtime,
    c.resolution,
    CASE
        WHEN c.resolution <> 'unresolved' THEN 'resolved'
        WHEN r.state IS NOT NULL THEN r.state
        ELSE 'unresolved'
    END,
    r.requested_resolution,
    r.requested_at,
    r.resolving_at,
    r.resolution_error,
    c.resolved_at,
    c.resolved_by
FROM conflicts_facts c
LEFT JOIN conflict_requests r ON r.conflict_id = c.id;

DROP TABLE conflict_requests;
DROP TABLE conflicts_facts;

CREATE INDEX idx_conflicts_resolution ON conflicts(resolution);
CREATE INDEX idx_conflicts_state ON conflicts(state);
