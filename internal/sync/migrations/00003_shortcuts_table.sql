-- +goose Up

-- Shortcut registry for shared folder sync (Phase 6.4).
-- Each row represents a OneDrive shortcut ("Add shortcut to My files") or
-- shared folder that requires separate observation on the source drive.
-- The observation column tracks whether this scope uses folder-scoped delta
-- (Personal source drives) or recursive children enumeration (Business/SharePoint).
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

-- +goose Down
DROP TABLE IF EXISTS shortcuts;
