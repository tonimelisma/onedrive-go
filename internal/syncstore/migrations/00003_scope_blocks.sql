-- +goose Up
-- scope_blocks persists active scope-level failure blocks so they survive
-- crashes. ScopeGate loads all rows at startup and maintains a write-through
-- in-memory cache. Typically 0-5 rows.
--
-- No FK to sync_failures — intentional. sync_failures rows must survive
-- scope_blocks deletion (onScopeClear queries them AFTER deleting the
-- scope_blocks row). Per-item failures may also have empty scope_key.
CREATE TABLE scope_blocks (
    scope_key      TEXT PRIMARY KEY,
    issue_type     TEXT NOT NULL,
    blocked_at     INTEGER NOT NULL,     -- unix nanos
    trial_interval INTEGER NOT NULL,     -- nanoseconds
    next_trial_at  INTEGER NOT NULL,     -- unix nanos
    trial_count    INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS scope_blocks;
