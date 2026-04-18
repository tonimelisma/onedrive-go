package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

const (
	recreateDriveStateDBHint = "delete the existing state DB and rerun sync; startup recreates a fresh canonical store automatically"
	canonicalSchemaSQL       = `
CREATE TABLE IF NOT EXISTS baseline (
    item_id         TEXT    NOT NULL PRIMARY KEY,
    path            TEXT    NOT NULL UNIQUE,
    parent_id       TEXT,
    item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    local_hash      TEXT,
    remote_hash     TEXT,
    local_size      INTEGER,
    remote_size     INTEGER,
    local_mtime     INTEGER,
    remote_mtime    INTEGER,
    etag            TEXT
);

CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id);

CREATE TABLE IF NOT EXISTS observation_state (
    singleton_id                  INTEGER PRIMARY KEY CHECK(singleton_id = 1),
    configured_drive_id           TEXT    NOT NULL DEFAULT '',
    cursor                        TEXT    NOT NULL DEFAULT '',
    remote_refresh_mode           TEXT    NOT NULL DEFAULT 'delta_healthy',
    last_full_remote_refresh_at   INTEGER NOT NULL DEFAULT 0,
    next_full_remote_refresh_at   INTEGER NOT NULL DEFAULT 0,
    local_refresh_mode            TEXT    NOT NULL DEFAULT 'watch_healthy',
    last_full_local_refresh_at    INTEGER NOT NULL DEFAULT 0,
    next_full_local_refresh_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS run_status (
    singleton_id           INTEGER PRIMARY KEY CHECK(singleton_id = 1),
    last_completed_at      INTEGER NOT NULL DEFAULT 0,
    last_duration_ms       INTEGER NOT NULL DEFAULT 0,
    last_succeeded_count   INTEGER NOT NULL DEFAULT 0,
    last_failed_count      INTEGER NOT NULL DEFAULT 0,
    last_error             TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS remote_state (
    item_id       TEXT    NOT NULL PRIMARY KEY,
    path          TEXT    NOT NULL UNIQUE,
    parent_id     TEXT,
    item_type     TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash          TEXT,
    size          INTEGER,
    mtime         INTEGER,
    etag          TEXT,
    content_identity TEXT,
    previous_path TEXT
);

CREATE INDEX IF NOT EXISTS idx_remote_state_parent ON remote_state(parent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_path ON remote_state(path);

CREATE TABLE IF NOT EXISTS local_state (
    path             TEXT    NOT NULL PRIMARY KEY,
    item_type        TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash             TEXT,
    size             INTEGER,
    mtime            INTEGER,
    content_identity TEXT,
    observed_at      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS retry_state (
    work_key        TEXT    NOT NULL PRIMARY KEY,
    path            TEXT    NOT NULL,
    old_path        TEXT    NOT NULL DEFAULT '',
    action_type     TEXT    NOT NULL,
    scope_key       TEXT    NOT NULL DEFAULT '',
    blocked         INTEGER NOT NULL DEFAULT 0 CHECK(blocked IN (0, 1)),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at   INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    first_seen_at   INTEGER NOT NULL DEFAULT 0,
    last_seen_at    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_retry_state_scope_key ON retry_state(scope_key);
CREATE INDEX IF NOT EXISTS idx_retry_state_blocked ON retry_state(blocked);

CREATE TABLE IF NOT EXISTS sync_failures (
    path           TEXT    NOT NULL PRIMARY KEY,
    direction      TEXT    NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
    action_type    TEXT    NOT NULL CHECK(action_type IN (
                    'download', 'upload', 'local_delete', 'remote_delete',
                    'local_move', 'remote_move', 'folder_create', 'conflict_copy',
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
            'conflict_copy', 'update_synced', 'cleanup'
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
    )
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

CREATE TABLE IF NOT EXISTS scope_blocks (
    scope_key      TEXT PRIMARY KEY,
    issue_type     TEXT NOT NULL,
    timing_source  TEXT NOT NULL CHECK(timing_source IN ('none', 'backoff', 'server_retry_after')),
    blocked_at     INTEGER NOT NULL,
    trial_interval INTEGER NOT NULL,
    next_trial_at  INTEGER NOT NULL,
    preserve_until INTEGER NOT NULL DEFAULT 0,
    trial_count    INTEGER NOT NULL DEFAULT 0
);`
)

// ErrIncompatibleSchema marks a state DB that cannot be trusted under the
// current canonical schema. The state is rebuildable, so incompatible stores
// fail loudly instead of being guessed at or partially imported.
var ErrIncompatibleSchema = errors.New("sync: incompatible sync store schema")

func canonicalSyncStoreColumns() map[string][]string {
	return map[string][]string{
		"baseline": {
			"item_id", "path", "parent_id", "item_type", "local_hash", "remote_hash",
			"local_size", "remote_size", "local_mtime", "remote_mtime", "etag",
		},
		"observation_state": {
			"singleton_id", "configured_drive_id", "cursor", "remote_refresh_mode",
			"last_full_remote_refresh_at", "next_full_remote_refresh_at",
			"local_refresh_mode", "last_full_local_refresh_at", "next_full_local_refresh_at",
		},
		"run_status": {
			"singleton_id", "last_completed_at", "last_duration_ms", "last_succeeded_count", "last_failed_count", "last_error",
		},
		"remote_state": {
			"item_id", "path", "parent_id", "item_type", "hash", "size", "mtime", "etag", "content_identity", "previous_path",
		},
		"local_state": {
			"path", "item_type", "hash", "size", "mtime", "content_identity", "observed_at",
		},
		"retry_state": {
			"work_key", "path", "old_path", "action_type", "scope_key", "blocked",
			"attempt_count", "next_retry_at", "last_error", "first_seen_at", "last_seen_at",
		},
		"sync_failures": {
			"path", "direction", "action_type", "category", "failure_role", "issue_type", "item_id",
			"failure_count", "next_retry_at", "last_error", "http_status", "first_seen_at", "last_seen_at",
			"file_size", "local_hash", "scope_key",
		},
		"scope_blocks": {
			"scope_key", "issue_type", "timing_source", "blocked_at", "trial_interval", "next_trial_at", "preserve_until", "trial_count",
		},
	}
}

func applySchema(ctx context.Context, db *sql.DB) error {
	userTables, err := listUserTables(ctx, db)
	if err != nil {
		return fmt.Errorf("sync: inspect schema tables: %w", err)
	}

	if len(userTables) == 0 {
		if err := createCanonicalSchema(ctx, db); err != nil {
			return fmt.Errorf("sync: create canonical schema: %w", err)
		}
		return nil
	}

	if err := validateCanonicalSchema(ctx, db, userTables); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return fmt.Errorf("sync: ensuring observation_state row: %w", err)
	}
	if _, err := db.ExecContext(ctx, sqlEnsureRunStatusRow); err != nil {
		return fmt.Errorf("sync: ensuring run_status row: %w", err)
	}

	return nil
}

func createCanonicalSchema(ctx context.Context, db *sql.DB) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	if _, err = tx.ExecContext(ctx, canonicalSchemaSQL); err != nil {
		return fmt.Errorf("apply schema DDL: %w", err)
	}
	if _, err = tx.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return fmt.Errorf("seed observation_state row: %w", err)
	}
	if _, err = tx.ExecContext(ctx, sqlEnsureRunStatusRow); err != nil {
		return fmt.Errorf("seed run_status row: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit schema transaction: %w", err)
	}

	return nil
}

func validateCanonicalSchema(ctx context.Context, db *sql.DB, actualTables []string) error {
	expectedColumnsByTable := canonicalSyncStoreColumns()
	expectedTables := make([]string, 0, len(expectedColumnsByTable))
	for tableName := range expectedColumnsByTable {
		expectedTables = append(expectedTables, tableName)
	}
	slices.Sort(expectedTables)
	slices.Sort(actualTables)

	if !slices.Equal(expectedTables, actualTables) {
		return fmt.Errorf(
			"%w: sync store tables do not match the current schema; found %v, expected %v; %s",
			ErrIncompatibleSchema,
			actualTables,
			expectedTables,
			recreateDriveStateDBHint,
		)
	}

	for _, tableName := range expectedTables {
		actualColumns, err := listTableColumns(ctx, db, tableName)
		if err != nil {
			return fmt.Errorf("sync: inspect schema columns for %s: %w", tableName, err)
		}

		expectedColumns := append([]string(nil), expectedColumnsByTable[tableName]...)
		slices.Sort(expectedColumns)
		slices.Sort(actualColumns)
		if !slices.Equal(expectedColumns, actualColumns) {
			return fmt.Errorf(
				"%w: sync store table %s does not match the current schema; found columns %v, expected %v; %s",
				ErrIncompatibleSchema,
				tableName,
				actualColumns,
				expectedColumns,
				recreateDriveStateDBHint,
			)
		}
	}

	return nil
}

func listUserTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name
		FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query sqlite_master tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			return nil, fmt.Errorf("scan sqlite_master table row: %w", err)
		}
		tables = append(tables, tableName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite_master tables: %w", err)
	}

	return tables, nil
}

func listTableColumns(ctx context.Context, db *sql.DB, tableName string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+tableName+`)`)
	if err != nil {
		return nil, fmt.Errorf("query table info for %s: %w", tableName, err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			pk           int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("scan table info for %s: %w", tableName, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table info for %s: %w", tableName, err)
	}

	return columns, nil
}
