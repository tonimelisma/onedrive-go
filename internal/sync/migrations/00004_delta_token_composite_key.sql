-- +goose Up

-- Recreate delta_tokens with composite primary key (drive_id, scope_id).
-- Existing tokens get scope_id = "" and scope_drive = drive_id (primary delta).
-- The scope_id/scope_drive columns enable per-shortcut delta token storage
-- for shared folder support (see MULTIDRIVE.md §8).

-- Step 1: Rename existing table.
ALTER TABLE delta_tokens RENAME TO _delta_tokens_old;

-- Step 2: Create new table with composite key.
CREATE TABLE delta_tokens (
    drive_id    TEXT    NOT NULL,
    scope_id    TEXT    NOT NULL DEFAULT '',
    scope_drive TEXT    NOT NULL DEFAULT '',
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0),
    PRIMARY KEY (drive_id, scope_id)
);

-- Step 3: Migrate existing rows. Primary deltas get scope_id = "" and
-- scope_drive = drive_id.
INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, token, updated_at)
SELECT drive_id, '', drive_id, token, updated_at FROM _delta_tokens_old;

-- Step 4: Drop old table.
DROP TABLE _delta_tokens_old;

-- +goose Down
-- Recreate original single-key table. Shortcut-scoped tokens are lost.
ALTER TABLE delta_tokens RENAME TO _delta_tokens_composite;

CREATE TABLE delta_tokens (
    drive_id    TEXT    PRIMARY KEY,
    token       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL CHECK(updated_at > 0)
);

-- Only primary tokens (scope_id = "") are preserved on rollback.
INSERT INTO delta_tokens (drive_id, token, updated_at)
SELECT drive_id, token, updated_at FROM _delta_tokens_composite WHERE scope_id = '';

DROP TABLE _delta_tokens_composite;
