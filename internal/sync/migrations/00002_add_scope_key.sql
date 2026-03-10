-- +goose Up
ALTER TABLE sync_failures ADD COLUMN scope_key TEXT NOT NULL DEFAULT '';

-- +goose Down
-- No backwards compatibility needed (pre-launch). Down migration omitted.
