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
	// Generation 17 adds local truth confidence fields to observation_state so
	// incremental watch commits can distinguish complete local truth from a
	// suspect snapshot that needs a full local refresh.
	//
	// Generation 18 adds pending_creates: new files whose empty item exists on
	// OneDrive while their content upload has not committed (R-5.6.15).
	//
	// Generation 19 records the sync folder's fingerprint in observation_state,
	// so a replaced or unmounted folder is refused instead of scanned as a
	// mass deletion, and adds held_deletes, the deletes the delete-safety
	// hold keeps for confirmation, and mount_boundaries, the folders a scan
	// crossed into another filesystem under cross_filesystems.
	//
	// Generation 20 records the local inode change time (baseline.local_ctime,
	// local_state.ctime) so the hash fast path can see an in-place edit that
	// restored the old mtime (R-2.2.4).
	//
	// Generation 21 counts consecutive full refreshes that left an orphan
	// undecided (observation_state.undecided_refresh_streak), so the refresh
	// it pulls forward backs off instead of re-enumerating every 15 minutes.
	//
	// Generation 22 adds the entry's birth time to local filesystem identity
	// (baseline.local_birth_time, local_state.local_birth_time,
	// shortcut_roots.local_root_birth_time): a reused inode number is not the
	// deleted entry it once named, and pairing on it renamed an unrelated new
	// entry into the old OneDrive item (R-2.17.1).
	//
	// Generation 23 adds the sync folder's own birth time to its recorded
	// identity (observation_state.sync_root_birth_time): a folder removed and
	// re-created at the sync path can get the old inode back, and on a
	// filesystem without the bind token's extended attribute that new, empty
	// folder passed as the bound one (D-001).
	//
	// Generation 24 adds kept_local_folders: folders deleted on OneDrive that
	// stay locally only because they hold content sync ignores (D-012). The
	// next pass used to read such a folder as new local content and re-create
	// it, empty, online (R-2.17.2).
	currentSyncStoreGeneration = 24
	sqlEnsureStoreMetadataRow  = `INSERT INTO store_metadata (schema_generation)
		SELECT ?
		WHERE NOT EXISTS (SELECT 1 FROM store_metadata)`
	canonicalSchemaSQL = `
CREATE TABLE IF NOT EXISTS store_metadata (
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
    local_ctime     INTEGER NOT NULL DEFAULT 0,
    local_device    INTEGER NOT NULL DEFAULT 0,
    local_inode     INTEGER NOT NULL DEFAULT 0,
    local_birth_time INTEGER NOT NULL DEFAULT 0,
    local_has_identity INTEGER NOT NULL DEFAULT 0 CHECK(local_has_identity IN (0, 1)),
    etag            TEXT
);

CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id);

CREATE TABLE IF NOT EXISTS observation_state (
    content_drive_id         TEXT    NOT NULL DEFAULT '',
    cursor                      TEXT    NOT NULL DEFAULT '',
    next_full_remote_refresh_at INTEGER NOT NULL DEFAULT 0,
    local_truth_complete        INTEGER NOT NULL DEFAULT 0 CHECK(local_truth_complete IN (0, 1)),
    local_truth_recovery_reason TEXT    NOT NULL DEFAULT '',
    sync_root_bound             INTEGER NOT NULL DEFAULT 0 CHECK(sync_root_bound IN (0, 1)),
    sync_root_device            INTEGER NOT NULL DEFAULT 0,
    sync_root_inode             INTEGER NOT NULL DEFAULT 0,
    sync_root_fsid              INTEGER NOT NULL DEFAULT 0,
    sync_root_birth_time        INTEGER NOT NULL DEFAULT 0,
    sync_root_token             TEXT    NOT NULL DEFAULT '',
    sync_root_rebind            INTEGER NOT NULL DEFAULT 0 CHECK(sync_root_rebind IN (0, 1)),
    undecided_refresh_streak    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS pending_creates (
    item_id       TEXT    NOT NULL PRIMARY KEY,
    drive_id      TEXT    NOT NULL DEFAULT '',
    path          TEXT    NOT NULL,
    parent_id     TEXT    NOT NULL DEFAULT '',
    created_etag  TEXT    NOT NULL DEFAULT '',
    local_size    INTEGER NOT NULL DEFAULT 0,
    local_hash    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_pending_creates_path ON pending_creates(path);

CREATE TABLE IF NOT EXISTS mount_boundaries (
    path       TEXT    NOT NULL PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS kept_local_folders (
    path             TEXT    NOT NULL PRIMARY KEY,
    local_device     INTEGER NOT NULL DEFAULT 0,
    local_inode      INTEGER NOT NULL DEFAULT 0,
    local_birth_time INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS held_deletes (
    path       TEXT    NOT NULL,
    side       TEXT    NOT NULL CHECK(side IN ('local', 'remote')),
    reason     TEXT    NOT NULL DEFAULT '',
    item_type  TEXT    NOT NULL DEFAULT 'file',
    item_id    TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (path, side)
);

CREATE TABLE IF NOT EXISTS remote_state (
    drive_id      TEXT    NOT NULL DEFAULT '',
    item_id       TEXT    NOT NULL PRIMARY KEY,
    path          TEXT    NOT NULL UNIQUE,
    item_type     TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash          TEXT,
    size          INTEGER,
    mtime         INTEGER,
    etag          TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_state_path ON remote_state(path);

CREATE TABLE IF NOT EXISTS local_state (
    path       TEXT    NOT NULL PRIMARY KEY,
    item_type  TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
    hash       TEXT,
    size       INTEGER,
    mtime      INTEGER,
    ctime      INTEGER NOT NULL DEFAULT 0,
    local_device INTEGER NOT NULL DEFAULT 0,
    local_inode  INTEGER NOT NULL DEFAULT 0,
    local_birth_time INTEGER NOT NULL DEFAULT 0,
    local_has_identity INTEGER NOT NULL DEFAULT 0 CHECK(local_has_identity IN (0, 1))
);

CREATE TABLE IF NOT EXISTS retry_work (
    path            TEXT    NOT NULL,
    old_path        TEXT    NOT NULL DEFAULT '',
    action_type     TEXT    NOT NULL,
    scope_key       TEXT    NOT NULL DEFAULT '',
    blocked         INTEGER NOT NULL DEFAULT 0 CHECK(blocked IN (0, 1)),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (path, old_path, action_type)
);

CREATE INDEX IF NOT EXISTS idx_retry_work_scope_key ON retry_work(scope_key);
CREATE INDEX IF NOT EXISTS idx_retry_work_blocked ON retry_work(blocked);
CREATE INDEX IF NOT EXISTS idx_retry_work_retrying ON retry_work(attempt_count)
    WHERE blocked = 0 AND attempt_count >= 3;

CREATE TABLE IF NOT EXISTS observation_issues (
    path           TEXT    NOT NULL PRIMARY KEY,
    issue_type     TEXT    NOT NULL DEFAULT '',
    scope_key      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_observation_issues_issue_type
    ON observation_issues(issue_type);
CREATE INDEX IF NOT EXISTS idx_observation_issues_scope_key
    ON observation_issues(scope_key);

CREATE TABLE IF NOT EXISTS block_scopes (
    scope_key      TEXT PRIMARY KEY,
    trial_interval INTEGER NOT NULL,
    next_trial_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS shortcut_roots (
    binding_item_id                  TEXT    NOT NULL PRIMARY KEY,
    namespace_id                     TEXT    NOT NULL DEFAULT '',
    relative_local_path              TEXT    NOT NULL DEFAULT '',
    local_alias                      TEXT    NOT NULL DEFAULT '',
    remote_drive_id                  TEXT    NOT NULL DEFAULT '',
    remote_item_id                   TEXT    NOT NULL DEFAULT '',
    remote_is_folder                 INTEGER NOT NULL DEFAULT 0 CHECK(remote_is_folder IN (0, 1)),
    state                            TEXT    NOT NULL DEFAULT '',
    protected_paths_json             TEXT    NOT NULL DEFAULT '[]',
    blocked_detail                   TEXT    NOT NULL DEFAULT '',
    local_root_device                INTEGER NOT NULL DEFAULT 0,
    local_root_inode                 INTEGER NOT NULL DEFAULT 0,
    local_root_birth_time            INTEGER NOT NULL DEFAULT 0,
    local_root_has_identity          INTEGER NOT NULL DEFAULT 0 CHECK(local_root_has_identity IN (0, 1)),
    waiting_binding_item_id          TEXT    NOT NULL DEFAULT '',
    waiting_relative_local_path      TEXT    NOT NULL DEFAULT '',
    waiting_local_alias              TEXT    NOT NULL DEFAULT '',
    waiting_remote_drive_id          TEXT    NOT NULL DEFAULT '',
    waiting_remote_item_id           TEXT    NOT NULL DEFAULT '',
    waiting_remote_is_folder         INTEGER NOT NULL DEFAULT 0 CHECK(waiting_remote_is_folder IN (0, 1))
);`
)

type storeCompatibilityMetadata struct {
	SchemaGeneration int
}

// errIncompatibleSchema marks a state DB that cannot be trusted under the
// current canonical schema. The state is rebuildable, so incompatible stores
// fail loudly instead of being guessed at or partially imported.
var errIncompatibleSchema = errors.New("sync: incompatible sync store schema")

func canonicalSyncStoreColumns() map[string][]string {
	return map[string][]string{
		"store_metadata": {
			"schema_generation",
		},
		"baseline": {
			"item_id", "path", "parent_id", "item_type", "local_hash", "remote_hash",
			"local_size", "remote_size", "local_mtime", "remote_mtime", "local_ctime",
			"local_device", "local_inode", "local_birth_time", "local_has_identity", "etag",
		},
		"observation_state": {
			"content_drive_id", "cursor", "next_full_remote_refresh_at",
			"local_truth_complete", "local_truth_recovery_reason",
			"sync_root_bound", "sync_root_device", "sync_root_inode", "sync_root_fsid",
			"sync_root_birth_time", "sync_root_token", "sync_root_rebind", "undecided_refresh_streak",
		},
		"remote_state": {
			"drive_id", "item_id", "path", "item_type", "hash", "size", "mtime", "etag",
		},
		"pending_creates": {
			"item_id", "drive_id", "path", "parent_id", "created_etag", "local_size", "local_hash",
		},
		"mount_boundaries": {
			"path",
		},
		"kept_local_folders": {
			"path", "local_device", "local_inode", "local_birth_time",
		},
		"held_deletes": {
			"path", "side", "reason", "item_type", "item_id",
		},
		"local_state": {
			"path", "item_type", "hash", "size", "mtime", "ctime",
			"local_device", "local_inode", "local_birth_time", "local_has_identity",
		},
		"retry_work": {
			"path", "old_path", "action_type", "scope_key", "blocked", "attempt_count", "next_retry_at",
		},
		"observation_issues": {
			"path", "issue_type", "scope_key",
		},
		"block_scopes": {
			"scope_key", "trial_interval", "next_trial_at",
		},
		"shortcut_roots": {
			"binding_item_id", "namespace_id", "relative_local_path", "local_alias",
			"remote_drive_id", "remote_item_id", "remote_is_folder", "state",
			"protected_paths_json", "blocked_detail", "local_root_device", "local_root_inode",
			"local_root_birth_time", "local_root_has_identity", "waiting_binding_item_id", "waiting_relative_local_path",
			"waiting_local_alias", "waiting_remote_drive_id", "waiting_remote_item_id",
			"waiting_remote_is_folder",
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
			errIncompatibleSchema,
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
				errIncompatibleSchema,
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
			errIncompatibleSchema,
			metadata.SchemaGeneration,
			currentSyncStoreGeneration,
		)
	}

	return nil
}

func readStoreCompatibilityMetadata(ctx context.Context, db *sql.DB) (storeCompatibilityMetadata, error) {
	metadata := storeCompatibilityMetadata{}
	err := db.QueryRowContext(ctx,
		`SELECT schema_generation FROM store_metadata LIMIT 1`,
	).Scan(&metadata.SchemaGeneration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storeCompatibilityMetadata{}, fmt.Errorf("%w: sync store generation marker is missing", errIncompatibleSchema)
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
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

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
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanTableColumns(rows, tableName)
}

func scanTableColumns(rows *sql.Rows, tableName string) ([]string, error) {
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
