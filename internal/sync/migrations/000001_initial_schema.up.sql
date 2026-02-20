-- Initial schema for onedrive-go sync state database.
-- See data-model.md for complete specification.

CREATE TABLE IF NOT EXISTS items (
    -- Identity (architecture section 6.3)
    drive_id        TEXT    NOT NULL,
    item_id         TEXT    NOT NULL,
    parent_drive_id TEXT,
    parent_id       TEXT,
    name            TEXT    NOT NULL,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root', 'remote')),
    path            TEXT,

    -- Remote state (from Graph API, updated by delta processor)
    size            INTEGER,
    etag            TEXT,
    ctag            TEXT,
    quick_xor_hash  TEXT,
    sha256_hash     TEXT,
    remote_mtime    INTEGER,

    -- Local state (from filesystem scanner)
    local_size      INTEGER,
    local_mtime     INTEGER,
    local_hash      TEXT,

    -- Sync base state (snapshot at last successful sync)
    synced_size     INTEGER,
    synced_mtime    INTEGER,
    synced_hash     TEXT,
    last_synced_at  INTEGER,

    -- Shared/remote item references
    remote_drive_id TEXT,
    remote_id       TEXT,

    -- Tombstone fields
    is_deleted      INTEGER NOT NULL DEFAULT 0 CHECK(is_deleted IN (0, 1)),
    deleted_at      INTEGER,

    -- Row metadata
    created_at      INTEGER NOT NULL CHECK(created_at > 0),
    updated_at      INTEGER NOT NULL CHECK(updated_at > 0),

    PRIMARY KEY (drive_id, item_id)
);

CREATE TABLE IF NOT EXISTS delta_tokens (
    drive_id   TEXT    PRIMARY KEY,
    token      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL CHECK(updated_at > 0)
);

-- Delta completeness tracking (sync-algorithm.md section 3.6).
-- Separate from delta_tokens because completeness must persist even when
-- the token is deleted on HTTP 410.
CREATE TABLE IF NOT EXISTS delta_complete (
    drive_id TEXT    PRIMARY KEY,
    complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN (0, 1))
);

CREATE TABLE IF NOT EXISTS conflicts (
    id            TEXT    PRIMARY KEY,
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL,
    detected_at   INTEGER NOT NULL CHECK(detected_at > 0),
    local_hash    TEXT,
    remote_hash   TEXT,
    local_mtime   INTEGER,
    remote_mtime  INTEGER,
    resolution    TEXT    NOT NULL DEFAULT 'unresolved'
                         CHECK(resolution IN (
                             'unresolved', 'keep_both', 'keep_local',
                             'keep_remote', 'manual'
                         )),
    resolved_at   INTEGER,
    resolved_by   TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL),
    history       TEXT,

    FOREIGN KEY (drive_id, item_id) REFERENCES items(drive_id, item_id)
);

CREATE TABLE IF NOT EXISTS stale_files (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL,
    reason      TEXT    NOT NULL,
    detected_at INTEGER NOT NULL CHECK(detected_at > 0),
    size        INTEGER
);

CREATE TABLE IF NOT EXISTS upload_sessions (
    id             TEXT    PRIMARY KEY,
    drive_id       TEXT    NOT NULL,
    item_id        TEXT,
    local_path     TEXT    NOT NULL,
    session_url    TEXT    NOT NULL,
    expiry         INTEGER NOT NULL CHECK(expiry > 0),
    bytes_uploaded INTEGER NOT NULL DEFAULT 0 CHECK(bytes_uploaded >= 0),
    total_size     INTEGER NOT NULL CHECK(total_size > 0),
    created_at     INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TABLE IF NOT EXISTS config_snapshot (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Indexes (data-model.md section 9)
CREATE INDEX IF NOT EXISTS idx_items_path ON items(path);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_drive_id, parent_id);
CREATE INDEX IF NOT EXISTS idx_items_tombstone ON items(is_deleted, deleted_at) WHERE is_deleted = 1;
CREATE INDEX IF NOT EXISTS idx_items_type ON items(item_type);
CREATE INDEX IF NOT EXISTS idx_conflicts_item ON conflicts(drive_id, item_id);
