package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

const (
	// currentSyncStoreGeneration is the compatibility contract for existing
	// state DBs. store_metadata owns this store-level marker; startup accepts
	// only the current generation and requires an explicit reset otherwise.
	//
	// Generation 6 renames cross-authority durable condition columns from
	// issue_type to condition_type and keeps parsed scope semantics alongside
	// scope_key so runtime, store, planner, and read-side code share one
	// validated scope descriptor shape.
	currentSyncStoreGeneration = 6
	sqlEnsureStoreMetadataRow  = `INSERT INTO store_metadata
		(singleton_id, schema_generation)
	VALUES (1, ?)
	ON CONFLICT(singleton_id) DO UPDATE SET schema_generation = excluded.schema_generation`
	canonicalSchemaSQL = `
CREATE TABLE IF NOT EXISTS store_metadata (
    singleton_id       INTEGER PRIMARY KEY CHECK(singleton_id = 1),
    schema_generation  INTEGER NOT NULL
);

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

CREATE TABLE IF NOT EXISTS retry_work (
    work_key        TEXT    NOT NULL PRIMARY KEY,
    path            TEXT    NOT NULL,
    old_path        TEXT    NOT NULL DEFAULT '',
    action_type     TEXT    NOT NULL,
    condition_type  TEXT    NOT NULL DEFAULT '',
    scope_key       TEXT    NOT NULL DEFAULT '',
    blocked         INTEGER NOT NULL DEFAULT 0 CHECK(blocked IN (0, 1)),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at   INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    http_status     INTEGER NOT NULL DEFAULT 0,
    first_seen_at   INTEGER NOT NULL DEFAULT 0,
    last_seen_at    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_retry_work_scope_key ON retry_work(scope_key);
CREATE INDEX IF NOT EXISTS idx_retry_work_blocked ON retry_work(blocked);
CREATE INDEX IF NOT EXISTS idx_retry_work_retrying ON retry_work(attempt_count)
    WHERE blocked = 0 AND attempt_count >= 3;

CREATE TABLE IF NOT EXISTS observation_issues (
    path           TEXT    NOT NULL PRIMARY KEY,
    action_type    TEXT    NOT NULL CHECK(action_type IN (
                    'download', 'upload', 'local_delete', 'remote_delete',
                    'local_move', 'remote_move', 'folder_create', 'conflict_copy',
                    'update_synced', 'cleanup')),
    issue_type     TEXT    NOT NULL DEFAULT '',
    item_id        TEXT    NOT NULL DEFAULT '',
    last_error     TEXT    NOT NULL DEFAULT '',
    first_seen_at  INTEGER NOT NULL DEFAULT 0,
    last_seen_at   INTEGER NOT NULL DEFAULT 0,
    file_size      INTEGER NOT NULL DEFAULT 0,
    local_hash     TEXT    NOT NULL DEFAULT '',
    scope_key      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_observation_issues_issue_type
    ON observation_issues(issue_type, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_observation_issues_scope_key
    ON observation_issues(scope_key);

CREATE TABLE IF NOT EXISTS block_scopes (
    scope_key      TEXT PRIMARY KEY,
    scope_family   TEXT NOT NULL,
    scope_access   TEXT NOT NULL,
    subject_kind   TEXT NOT NULL,
    subject_value  TEXT NOT NULL DEFAULT '',
    condition_type TEXT NOT NULL,
    timing_source  TEXT NOT NULL CHECK(timing_source IN ('none', 'backoff', 'server_retry_after')),
    blocked_at     INTEGER NOT NULL,
    trial_interval INTEGER NOT NULL,
    next_trial_at  INTEGER NOT NULL,
    trial_count    INTEGER NOT NULL DEFAULT 0
		);`
)

type storeCompatibilityMetadata struct {
	SchemaGeneration int
}

// ErrIncompatibleSchema marks a state DB that cannot be trusted under the
// current canonical schema. The state is rebuildable, so incompatible stores
// fail loudly instead of being guessed at or partially imported.
var ErrIncompatibleSchema = errors.New("sync: incompatible sync store schema")

func canonicalSyncStoreColumns() map[string][]string {
	return map[string][]string{
		"store_metadata": {
			"singleton_id", "schema_generation",
		},
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
		"retry_work": {
			"work_key", "path", "old_path", "action_type", "condition_type", "scope_key", "blocked",
			"attempt_count", "next_retry_at", "last_error", "http_status", "first_seen_at", "last_seen_at",
		},
		"observation_issues": {
			"path", "action_type", "issue_type", "item_id", "last_error",
			"first_seen_at", "last_seen_at", "file_size", "local_hash", "scope_key",
		},
		"block_scopes": {
			"scope_key", "scope_family", "scope_access", "subject_kind", "subject_value",
			"condition_type", "timing_source", "blocked_at", "trial_interval", "next_trial_at", "trial_count",
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
	if metadataErr := ensureStoreCompatibilityMetadata(ctx, tx); metadataErr != nil {
		return metadataErr
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
			"%w: sync store tables do not match the current schema; found %v, expected %v",
			ErrIncompatibleSchema,
			actualTables,
			expectedTables,
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
				"%w: sync store table %s does not match the current schema; found columns %v, expected %v",
				ErrIncompatibleSchema,
				tableName,
				actualColumns,
				expectedColumns,
			)
		}
	}

	if err := validateStoreGeneration(ctx, db); err != nil {
		return err
	}

	return nil
}

func ensureStoreCompatibilityMetadata(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
},
) error {
	if _, err := exec.ExecContext(ctx, sqlEnsureStoreMetadataRow, currentSyncStoreGeneration); err != nil {
		return fmt.Errorf("seed store_metadata row: %w", err)
	}

	return nil
}

func validateStoreGeneration(ctx context.Context, db *sql.DB) error {
	metadata, err := readStoreCompatibilityMetadata(ctx, db)
	if err != nil {
		return err
	}
	if metadata.SchemaGeneration != currentSyncStoreGeneration {
		return fmt.Errorf(
			"%w: sync store generation %d is unsupported; expected %d",
			ErrIncompatibleSchema,
			metadata.SchemaGeneration,
			currentSyncStoreGeneration,
		)
	}

	return nil
}

func readStoreCompatibilityMetadata(ctx context.Context, db *sql.DB) (storeCompatibilityMetadata, error) {
	metadata := storeCompatibilityMetadata{}
	err := db.QueryRowContext(ctx,
		`SELECT schema_generation FROM store_metadata WHERE singleton_id = 1`,
	).Scan(&metadata.SchemaGeneration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storeCompatibilityMetadata{}, fmt.Errorf("%w: sync store generation marker is missing", ErrIncompatibleSchema)
		}
		return storeCompatibilityMetadata{}, fmt.Errorf("sync: inspect store compatibility metadata: %w", err)
	}

	return metadata, nil
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
