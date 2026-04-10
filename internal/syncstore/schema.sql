-- Canonical schema for the sync engine state database.
-- The project has no launched users and no state-compatibility burden, so the
-- schema is defined directly in its final shape. schema.go gates existing DBs
-- with PRAGMA user_version instead of running stepwise migrations.

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

-- Graph API delta cursor per drive with composite key for shared folder support.
CREATE TABLE IF NOT EXISTS delta_tokens (
    drive_id    TEXT    NOT NULL,
    scope_id    TEXT    NOT NULL DEFAULT '',
    scope_drive TEXT    NOT NULL DEFAULT '',
    cursor      TEXT    NOT NULL,
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

CREATE INDEX IF NOT EXISTS idx_conflicts_resolution ON conflicts(resolution);
CREATE INDEX IF NOT EXISTS idx_conflicts_state ON conflicts(state);

-- Big-delete protection ledger. These rows are user-gated safety decisions,
-- not sync failures: held rows wait for approval, approved rows are consumed
-- by the next engine pass without retriggering big-delete protection.
CREATE TABLE IF NOT EXISTS held_deletes (
    drive_id        TEXT    NOT NULL,
    action_type     TEXT    NOT NULL CHECK(action_type IN ('local_delete', 'remote_delete')),
    path            TEXT    NOT NULL,
    item_id         TEXT    NOT NULL CHECK(item_id <> ''),
    state           TEXT    NOT NULL CHECK(state IN ('held', 'approved')),
    held_at         INTEGER NOT NULL CHECK(held_at > 0),
    approved_at     INTEGER,
    last_planned_at INTEGER NOT NULL CHECK(last_planned_at > 0),
    last_error      TEXT,
    PRIMARY KEY (drive_id, action_type, path, item_id)
);

CREATE INDEX IF NOT EXISTS idx_held_deletes_state ON held_deletes(state);

-- Sync metadata key-value store for reporting.
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Durable sync-scope projection for the selected drive. The singleton row
-- tracks the last effective scope snapshot together with the observation
-- strategy currently in force.
CREATE TABLE IF NOT EXISTS scope_state (
    singleton               INTEGER PRIMARY KEY CHECK(singleton = 1),
    generation              INTEGER NOT NULL CHECK(generation >= 0),
    effective_snapshot_json TEXT    NOT NULL,
    observation_plan_hash   TEXT    NOT NULL,
    observation_mode        TEXT    NOT NULL CHECK(observation_mode IN (
                                'root_delta', 'scoped_delta', 'scoped_enumerate'
                            )),
    websocket_enabled       INTEGER NOT NULL DEFAULT 0 CHECK(websocket_enabled IN (0, 1)),
    pending_reentry         INTEGER NOT NULL DEFAULT 0 CHECK(pending_reentry IN (0, 1)),
    last_reconcile_kind     TEXT    NOT NULL CHECK(last_reconcile_kind IN (
                                'none', 'entered_paths', 'full'
                            )),
    updated_at              INTEGER NOT NULL CHECK(updated_at > 0)
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
    filter_generation INTEGER NOT NULL DEFAULT 0,
    filter_reason  TEXT    NOT NULL DEFAULT ''
                   CHECK(filter_reason IN ('', 'path_scope', 'marker_scope')),
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
