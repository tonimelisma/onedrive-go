-- +goose Up
CREATE TABLE action_queue (
    id           INTEGER PRIMARY KEY,
    cycle_id     TEXT    NOT NULL,
    action_type  TEXT    NOT NULL CHECK(action_type IN (
        'download','upload','local_delete','remote_delete',
        'local_move','remote_move','folder_create',
        'conflict','update_synced','cleanup')),
    path         TEXT    NOT NULL,
    old_path     TEXT,
    status       TEXT    NOT NULL DEFAULT 'pending'
                 CHECK(status IN ('pending','claimed','done','failed','canceled')),
    -- JSON array of planner-assigned indices (0-based positions in the actions
    -- slice), NOT ledger IDs. The DepTracker maps these indices to ledger IDs
    -- when building the in-memory dependency graph. For crash recovery, the
    -- mapping is deterministic: sequential IDs within a single-tx insert.
    depends_on   TEXT,
    drive_id     TEXT,
    item_id      TEXT,
    parent_id    TEXT,
    hash         TEXT,
    size         INTEGER,
    mtime        INTEGER,
    session_url  TEXT,
    bytes_done   INTEGER NOT NULL DEFAULT 0,
    claimed_at   INTEGER,
    completed_at INTEGER,
    error_msg    TEXT
);
CREATE INDEX idx_action_queue_status ON action_queue(status);
CREATE INDEX idx_action_queue_cycle  ON action_queue(cycle_id);
CREATE INDEX idx_action_queue_path   ON action_queue(path);

DROP TABLE IF EXISTS upload_sessions;

-- +goose Down
DROP TABLE IF EXISTS action_queue;
