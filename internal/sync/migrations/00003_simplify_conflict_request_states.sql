-- +goose Up
-- Collapse conflict request workflow to queued/applying while preserving the
-- last failed attempt message on queued requests.

ALTER TABLE conflict_requests RENAME TO conflict_requests_legacy;

CREATE TABLE conflict_requests (
    conflict_id          TEXT    PRIMARY KEY REFERENCES conflicts(id) ON DELETE CASCADE,
    requested_resolution TEXT    NOT NULL CHECK(requested_resolution IN (
                               'keep_both', 'keep_local', 'keep_remote'
                           )),
    state                TEXT    NOT NULL CHECK(state IN (
                               'queued', 'applying'
                           )),
    requested_at         INTEGER,
    applying_at          INTEGER,
    last_error           TEXT
);

INSERT INTO conflict_requests (
    conflict_id,
    requested_resolution,
    state,
    requested_at,
    applying_at,
    last_error
)
SELECT
    conflict_id,
    requested_resolution,
    CASE
        WHEN state = 'resolving' THEN 'applying'
        ELSE 'queued'
    END,
    requested_at,
    CASE
        WHEN state = 'resolving' THEN resolving_at
        ELSE NULL
    END,
    resolution_error
FROM conflict_requests_legacy;

DROP TABLE conflict_requests_legacy;

CREATE INDEX idx_conflict_requests_state ON conflict_requests(state);

-- +goose Down
-- Reconstruct the older queued/resolving/failed request workflow table.

ALTER TABLE conflict_requests RENAME TO conflict_requests_simple;

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

INSERT INTO conflict_requests (
    conflict_id,
    requested_resolution,
    state,
    requested_at,
    resolving_at,
    resolution_error
)
SELECT
    conflict_id,
    requested_resolution,
    CASE
        WHEN state = 'applying' THEN 'resolving'
        WHEN last_error IS NOT NULL AND last_error != '' THEN 'resolve_failed'
        ELSE 'resolution_requested'
    END,
    requested_at,
    CASE
        WHEN state = 'applying' THEN applying_at
        ELSE NULL
    END,
    last_error
FROM conflict_requests_simple;

DROP TABLE conflict_requests_simple;

CREATE INDEX idx_conflict_requests_state ON conflict_requests(state);
