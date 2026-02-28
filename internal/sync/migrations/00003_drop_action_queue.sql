-- +goose Up
DROP TABLE IF EXISTS action_queue;

-- Clean up unused forward-declaration tables from Phase 4 (B-092).
DROP TABLE IF EXISTS stale_files;
DROP TABLE IF EXISTS config_snapshots;
DROP TABLE IF EXISTS change_journal;

-- +goose Down
-- Intentionally empty: ledger is permanently removed.
