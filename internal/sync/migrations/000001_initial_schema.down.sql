-- Rollback: drop all tables and indexes created by the initial schema.

DROP INDEX IF EXISTS idx_conflicts_item;
DROP INDEX IF EXISTS idx_items_type;
DROP INDEX IF EXISTS idx_items_tombstone;
DROP INDEX IF EXISTS idx_items_parent;
DROP INDEX IF EXISTS idx_items_path;

DROP TABLE IF EXISTS config_snapshot;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS stale_files;
DROP TABLE IF EXISTS conflicts;
DROP TABLE IF EXISTS delta_complete;
DROP TABLE IF EXISTS delta_tokens;
DROP TABLE IF EXISTS items;
