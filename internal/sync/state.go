package sync

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Pure Go SQLite driver, registers as "sqlite".
)

// Embed migration SQL files for schema versioning.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Named constants for pragma values (mnd linter).
const (
	walJournalSizeLimit = 67108864 // 64 MiB WAL journal size limit
	schemaVersion       = 1        // current expected schema version
)

// SQLiteStore implements the Store interface using an embedded SQLite database
// with WAL mode. All sync state (items, delta tokens, conflicts, stale files,
// upload sessions, config snapshots) is persisted here.
type SQLiteStore struct {
	db     *sql.DB
	logger *slog.Logger

	// Prepared statements for repeated queries, grouped by domain.
	itemStmts     itemStatements
	deltaStmts    deltaStatements
	conflictStmts conflictStatements
	staleStmts    staleStatements
	uploadStmts   uploadStatements
	configStmts   configStatements
}

// Statement groups to avoid a flat list of 25+ fields.
type itemStatements struct {
	get, upsert, markDeleted, deleteByKey, listChildren, getByPath, listAllActive, listSynced *sql.Stmt
}

type deltaStatements struct {
	getToken, saveToken, deleteToken, setComplete, isComplete *sql.Stmt
}

type conflictStatements struct {
	record, list, resolve, count *sql.Stmt
}

type staleStatements struct {
	record, list, remove *sql.Stmt
}

type uploadStatements struct {
	save, get, delete, listExpired *sql.Stmt
}

type configStatements struct {
	get, save *sql.Stmt
}

// NewStore creates a new SQLiteStore, opening the database at dbPath, applying
// migrations, and preparing all repeated statements. Use ":memory:" for tests.
func NewStore(dbPath string, logger *slog.Logger) (*SQLiteStore, error) {
	logger.Info("opening sync state database", "path", dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := setPragmas(context.Background(), db, logger); err != nil {
		db.Close()
		return nil, err
	}

	if err := runMigrations(context.Background(), db, logger); err != nil {
		db.Close()
		return nil, err
	}

	s := &SQLiteStore{db: db, logger: logger}

	if err := s.prepareAllStatements(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare statements: %w", err)
	}

	logger.Info("sync state database ready", "path", dbPath)

	return s, nil
}

// setPragmas configures SQLite for WAL mode and safety.
func setPragmas(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	pragmas := []struct {
		sql  string
		desc string
	}{
		{"PRAGMA journal_mode = WAL", "WAL mode"},
		{"PRAGMA synchronous = FULL", "synchronous FULL"},
		{"PRAGMA foreign_keys = ON", "foreign keys"},
		{fmt.Sprintf("PRAGMA journal_size_limit = %d", walJournalSizeLimit), "journal size limit"},
	}

	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p.sql); err != nil {
			return fmt.Errorf("set pragma %s: %w", p.desc, err)
		}

		logger.Debug("pragma set", "pragma", p.desc)
	}

	return nil
}

// runMigrations applies embedded SQL migrations in order. Uses a simple
// migration runner instead of golang-migrate to avoid driver compatibility
// issues with the pure-Go SQLite driver.
func runMigrations(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	var currentVersion int

	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	logger.Debug("current schema version", "version", currentVersion)

	if currentVersion >= schemaVersion {
		logger.Debug("schema up to date", "version", currentVersion)
		return nil
	}

	for v := currentVersion + 1; v <= schemaVersion; v++ {
		if err := applyMigration(ctx, db, logger, v); err != nil {
			return err
		}
	}

	return nil
}

// applyMigration runs a single numbered up-migration inside a transaction.
func applyMigration(ctx context.Context, db *sql.DB, logger *slog.Logger, version int) error {
	filename := fmt.Sprintf("migrations/%06d_initial_schema.up.sql", version)

	migrationSQL, err := fs.ReadFile(migrationsFS, filename)
	if err != nil {
		return fmt.Errorf("read migration %d: %w", version, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx %d: %w", version, err)
	}

	if _, execErr := tx.ExecContext(ctx, string(migrationSQL)); execErr != nil {
		rollbackErr := tx.Rollback()
		return fmt.Errorf("exec migration %d: %w (rollback: %v)", version, execErr, rollbackErr)
	}

	// Stamp the new version. PRAGMA cannot be parameterized.
	versionSQL := fmt.Sprintf("PRAGMA user_version = %d", version)
	if _, execErr := tx.ExecContext(ctx, versionSQL); execErr != nil {
		rollbackErr := tx.Rollback()
		return fmt.Errorf("stamp version %d: %w (rollback: %v)", version, execErr, rollbackErr)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", version, err)
	}

	logger.Info("applied migration", "version", version, "file", filepath.Base(filename))

	return nil
}

// --- SQL query constants ---
// Multi-line to satisfy 140-character line limit. Grouped by domain.

// Item queries.
const (
	sqlItemColumns = `drive_id, item_id, parent_drive_id, parent_id, name,
		item_type, path, size, etag, ctag, quick_xor_hash, sha256_hash,
		remote_mtime, local_size, local_mtime, local_hash,
		synced_size, synced_mtime, synced_hash, last_synced_at,
		remote_drive_id, remote_id,
		is_deleted, deleted_at, created_at, updated_at`

	sqlGetItem = `SELECT ` + sqlItemColumns +
		` FROM items WHERE drive_id = ? AND item_id = ?`

	sqlUpsertItem = `INSERT INTO items (` + sqlItemColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, item_id) DO UPDATE SET
			parent_drive_id = excluded.parent_drive_id,
			parent_id       = excluded.parent_id,
			name            = excluded.name,
			item_type       = excluded.item_type,
			path            = excluded.path,
			size            = excluded.size,
			etag            = excluded.etag,
			ctag            = excluded.ctag,
			quick_xor_hash  = excluded.quick_xor_hash,
			sha256_hash     = excluded.sha256_hash,
			remote_mtime    = excluded.remote_mtime,
			local_size      = excluded.local_size,
			local_mtime     = excluded.local_mtime,
			local_hash      = excluded.local_hash,
			synced_size     = excluded.synced_size,
			synced_mtime    = excluded.synced_mtime,
			synced_hash     = excluded.synced_hash,
			last_synced_at  = excluded.last_synced_at,
			remote_drive_id = excluded.remote_drive_id,
			remote_id       = excluded.remote_id,
			is_deleted      = excluded.is_deleted,
			deleted_at      = excluded.deleted_at,
			updated_at      = excluded.updated_at`

	sqlMarkDeleted = `UPDATE items
		SET is_deleted = 1, deleted_at = ?, updated_at = ?
		WHERE drive_id = ? AND item_id = ?`

	sqlDeleteItemByKey = `DELETE FROM items WHERE drive_id = ? AND item_id = ?`

	sqlListChildren = `SELECT ` + sqlItemColumns +
		` FROM items
		WHERE parent_drive_id = ? AND parent_id = ? AND is_deleted = 0`

	sqlGetItemByPath = `SELECT ` + sqlItemColumns +
		` FROM items WHERE path = ? AND is_deleted = 0`

	sqlListAllActive = `SELECT ` + sqlItemColumns +
		` FROM items WHERE is_deleted = 0`

	sqlListSynced = `SELECT ` + sqlItemColumns +
		` FROM items WHERE synced_hash != '' AND is_deleted = 0`
)

// Delta token queries.
const (
	sqlGetDeltaToken = `SELECT token FROM delta_tokens WHERE drive_id = ?` //nolint:gosec // SQL column, not a credential

	sqlSaveDeltaToken = `INSERT INTO delta_tokens
		(drive_id, token, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(drive_id) DO UPDATE
		SET token = excluded.token, updated_at = excluded.updated_at`

	sqlDeleteDeltaToken = `DELETE FROM delta_tokens WHERE drive_id = ?`

	sqlSetDeltaComplete = `INSERT INTO delta_complete
		(drive_id, complete) VALUES (?, ?)
		ON CONFLICT(drive_id) DO UPDATE
		SET complete = excluded.complete`

	sqlIsDeltaComplete = `SELECT complete FROM delta_complete
		WHERE drive_id = ?`
)

// Conflict queries.
const (
	sqlRecordConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime,
		 resolution, history)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlListConflicts = `SELECT id, drive_id, item_id, path,
		detected_at, local_hash, remote_hash,
		local_mtime, remote_mtime, resolution,
		resolved_at, resolved_by, history
		FROM conflicts WHERE drive_id = ?`

	sqlResolveConflict = `UPDATE conflicts
		SET resolution = ?, resolved_at = ?, resolved_by = ?
		WHERE id = ?`

	sqlConflictCount = `SELECT COUNT(*) FROM conflicts
		WHERE drive_id = ? AND resolution = 'unresolved'`
)

// Stale file queries.
const (
	sqlRecordStaleFile = `INSERT INTO stale_files
		(id, path, reason, detected_at, size) VALUES (?, ?, ?, ?, ?)`

	sqlListStaleFiles = `SELECT id, path, reason, detected_at, size
		FROM stale_files`

	sqlRemoveStaleFile = `DELETE FROM stale_files WHERE id = ?`
)

// Upload session queries.
const (
	sqlSaveUploadSess = `INSERT INTO upload_sessions
		(id, drive_id, item_id, local_path, session_url,
		 expiry, bytes_uploaded, total_size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE
		SET bytes_uploaded = excluded.bytes_uploaded`

	sqlGetUploadSess = `SELECT id, drive_id, item_id, local_path,
		session_url, expiry, bytes_uploaded, total_size, created_at
		FROM upload_sessions WHERE id = ?`

	sqlDeleteUploadSess = `DELETE FROM upload_sessions WHERE id = ?`

	sqlListExpiredSess = `SELECT id, drive_id, item_id, local_path,
		session_url, expiry, bytes_uploaded, total_size, created_at
		FROM upload_sessions WHERE expiry < ?`
)

// Config snapshot queries.
const (
	sqlGetConfigSnap = `SELECT value FROM config_snapshot
		WHERE key = ?`

	sqlSaveConfigSnap = `INSERT INTO config_snapshot (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`
)

// stmtDef maps a SQL string to the prepared statement pointer it should populate.
// Used by the generic prepare helper to eliminate repetitive error handling.
type stmtDef struct {
	dest **sql.Stmt
	sql  string
	name string
}

// prepareAll prepares a batch of statements, returning on first error.
func prepareAll(ctx context.Context, db *sql.DB, defs []stmtDef) error {
	for i := range defs {
		stmt, err := db.PrepareContext(ctx, defs[i].sql)
		if err != nil {
			return fmt.Errorf("prepare %s: %w", defs[i].name, err)
		}

		*defs[i].dest = stmt
	}

	return nil
}

// prepareAllStatements creates all prepared statements grouped by domain.
func (s *SQLiteStore) prepareAllStatements(ctx context.Context) error {
	if err := s.prepareItemStmts(ctx); err != nil {
		return err
	}

	if err := s.prepareDeltaStmts(ctx); err != nil {
		return err
	}

	if err := s.prepareConflictStmts(ctx); err != nil {
		return err
	}

	if err := s.prepareStaleStmts(ctx); err != nil {
		return err
	}

	if err := s.prepareUploadStmts(ctx); err != nil {
		return err
	}

	return s.prepareConfigStmts(ctx)
}

func (s *SQLiteStore) prepareItemStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.itemStmts.get, sqlGetItem, "getItem"},
		{&s.itemStmts.upsert, sqlUpsertItem, "upsertItem"},
		{&s.itemStmts.markDeleted, sqlMarkDeleted, "markDeleted"},
		{&s.itemStmts.deleteByKey, sqlDeleteItemByKey, "deleteItemByKey"},
		{&s.itemStmts.listChildren, sqlListChildren, "listChildren"},
		{&s.itemStmts.getByPath, sqlGetItemByPath, "getItemByPath"},
		{&s.itemStmts.listAllActive, sqlListAllActive, "listAllActive"},
		{&s.itemStmts.listSynced, sqlListSynced, "listSynced"},
	})
}

func (s *SQLiteStore) prepareDeltaStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.deltaStmts.getToken, sqlGetDeltaToken, "getDeltaToken"},
		{&s.deltaStmts.saveToken, sqlSaveDeltaToken, "saveDeltaToken"},
		{&s.deltaStmts.deleteToken, sqlDeleteDeltaToken, "deleteDeltaToken"},
		{&s.deltaStmts.setComplete, sqlSetDeltaComplete, "setDeltaComplete"},
		{&s.deltaStmts.isComplete, sqlIsDeltaComplete, "isDeltaComplete"},
	})
}

func (s *SQLiteStore) prepareConflictStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.conflictStmts.record, sqlRecordConflict, "recordConflict"},
		{&s.conflictStmts.list, sqlListConflicts, "listConflicts"},
		{&s.conflictStmts.resolve, sqlResolveConflict, "resolveConflict"},
		{&s.conflictStmts.count, sqlConflictCount, "conflictCount"},
	})
}

func (s *SQLiteStore) prepareStaleStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.staleStmts.record, sqlRecordStaleFile, "recordStaleFile"},
		{&s.staleStmts.list, sqlListStaleFiles, "listStaleFiles"},
		{&s.staleStmts.remove, sqlRemoveStaleFile, "removeStaleFile"},
	})
}

func (s *SQLiteStore) prepareUploadStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.uploadStmts.save, sqlSaveUploadSess, "saveUploadSession"},
		{&s.uploadStmts.get, sqlGetUploadSess, "getUploadSession"},
		{&s.uploadStmts.delete, sqlDeleteUploadSess, "deleteUploadSession"},
		{&s.uploadStmts.listExpired, sqlListExpiredSess, "listExpiredSessions"},
	})
}

func (s *SQLiteStore) prepareConfigStmts(ctx context.Context) error {
	return prepareAll(ctx, s.db, []stmtDef{
		{&s.configStmts.get, sqlGetConfigSnap, "getConfigSnapshot"},
		{&s.configStmts.save, sqlSaveConfigSnap, "saveConfigSnapshot"},
	})
}

// --- Item scanning helpers ---

// scanItem scans a full item row from the database into an Item struct.
// Used by all item-returning queries to avoid duplicated column scanning.
func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	item := &Item{}

	err := row.Scan(
		&item.DriveID, &item.ItemID, &item.ParentDriveID, &item.ParentID,
		&item.Name, &item.ItemType, &item.Path,
		&item.Size, &item.ETag, &item.CTag, &item.QuickXorHash,
		&item.SHA256Hash, &item.RemoteMtime,
		&item.LocalSize, &item.LocalMtime, &item.LocalHash,
		&item.SyncedSize, &item.SyncedMtime, &item.SyncedHash, &item.LastSyncedAt,
		&item.RemoteDriveID, &item.RemoteID,
		&item.IsDeleted, &item.DeletedAt, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return item, nil
}

// scanItemRows iterates over sql.Rows and collects Items.
func scanItemRows(rows *sql.Rows) ([]*Item, error) {
	var items []*Item

	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item row: %w", err)
		}

		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate item rows: %w", err)
	}

	return items, nil
}

// upsertItemArgs returns the argument slice for the upsert prepared statement.
func upsertItemArgs(item *Item) []any {
	return []any{
		item.DriveID, item.ItemID, item.ParentDriveID, item.ParentID,
		item.Name, string(item.ItemType), item.Path,
		item.Size, item.ETag, item.CTag, item.QuickXorHash,
		item.SHA256Hash, item.RemoteMtime,
		item.LocalSize, item.LocalMtime, item.LocalHash,
		item.SyncedSize, item.SyncedMtime, item.SyncedHash, item.LastSyncedAt,
		item.RemoteDriveID, item.RemoteID,
		item.IsDeleted, item.DeletedAt, item.CreatedAt, item.UpdatedAt,
	}
}

// --- Item CRUD methods ---

// GetItem retrieves a single item by drive and item ID.
// Returns (nil, nil) if no item exists — callers (delta processor, executor)
// use the nil item to distinguish "new item" from "existing item".
func (s *SQLiteStore) GetItem(ctx context.Context, driveID, itemID string) (*Item, error) {
	s.logger.Debug("getting item", "drive_id", driveID, "item_id", itemID)

	item, err := scanItem(s.itemStmts.get.QueryRowContext(ctx, driveID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get item %s/%s: %w", driveID, itemID, err)
	}

	return item, nil
}

// UpsertItem inserts or updates an item in the state database.
func (s *SQLiteStore) UpsertItem(ctx context.Context, item *Item) error {
	s.logger.Debug("upserting item",
		"drive_id", item.DriveID, "item_id", item.ItemID, "name", item.Name)

	_, err := s.itemStmts.upsert.ExecContext(ctx, upsertItemArgs(item)...)
	if err != nil {
		return fmt.Errorf("upsert item %s/%s: %w", item.DriveID, item.ItemID, err)
	}

	return nil
}

// MarkDeleted sets the tombstone fields on an item.
func (s *SQLiteStore) MarkDeleted(ctx context.Context, driveID, itemID string, deletedAt int64) error {
	s.logger.Debug("marking item deleted", "drive_id", driveID, "item_id", itemID)

	now := NowNano()

	_, err := s.itemStmts.markDeleted.ExecContext(ctx, deletedAt, now, driveID, itemID)
	if err != nil {
		return fmt.Errorf("mark deleted %s/%s: %w", driveID, itemID, err)
	}

	return nil
}

// DeleteItemByKey physically removes an item by primary key.
// Used to clean up stale scanner-originated rows after upload assigns
// a server ItemID, preventing dual-row accumulation (B-050).
func (s *SQLiteStore) DeleteItemByKey(ctx context.Context, driveID, itemID string) error {
	s.logger.Debug("deleting item by key", "drive_id", driveID, "item_id", itemID)

	_, err := s.itemStmts.deleteByKey.ExecContext(ctx, driveID, itemID)
	if err != nil {
		return fmt.Errorf("delete item %s/%s: %w", driveID, itemID, err)
	}

	return nil
}

// ListChildren returns all non-deleted children of the given parent.
func (s *SQLiteStore) ListChildren(ctx context.Context, driveID, parentID string) ([]*Item, error) {
	s.logger.Debug("listing children", "drive_id", driveID, "parent_id", parentID)

	rows, err := s.itemStmts.listChildren.QueryContext(ctx, driveID, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children %s/%s: %w", driveID, parentID, err)
	}
	defer rows.Close()

	return scanItemRows(rows)
}

// GetItemByPath returns the first non-deleted item matching the given path.
// Returns (nil, nil) if no item exists at the path — callers (scanner, executor)
// use the nil item to distinguish "new file" from "known file".
func (s *SQLiteStore) GetItemByPath(ctx context.Context, path string) (*Item, error) {
	s.logger.Debug("getting item by path", "path", path)

	item, err := scanItem(s.itemStmts.getByPath.QueryRowContext(ctx, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get item by path %q: %w", path, err)
	}

	return item, nil
}

// ListAllActiveItems returns all non-deleted items in the database.
func (s *SQLiteStore) ListAllActiveItems(ctx context.Context) ([]*Item, error) {
	s.logger.Debug("listing all active items")

	rows, err := s.itemStmts.listAllActive.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active items: %w", err)
	}
	defer rows.Close()

	return scanItemRows(rows)
}

// ListSyncedItems returns all non-deleted items that have a synced hash
// (i.e., items that have been successfully synced at least once).
func (s *SQLiteStore) ListSyncedItems(ctx context.Context) ([]*Item, error) {
	s.logger.Debug("listing synced items")

	rows, err := s.itemStmts.listSynced.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list synced items: %w", err)
	}
	defer rows.Close()

	return scanItemRows(rows)
}

// BatchUpsert inserts or updates multiple items in a single transaction.
// This is significantly faster than individual upserts for delta processing.
func (s *SQLiteStore) BatchUpsert(ctx context.Context, items []*Item) error {
	s.logger.Debug("batch upserting items", "count", len(items))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch upsert tx: %w", err)
	}

	stmt := tx.StmtContext(ctx, s.itemStmts.upsert)

	for i := range items {
		if _, execErr := stmt.ExecContext(ctx, upsertItemArgs(items[i])...); execErr != nil {
			rollbackErr := tx.Rollback()
			return fmt.Errorf("batch upsert item %d (%s/%s): %w (rollback: %v)",
				i, items[i].DriveID, items[i].ItemID, execErr, rollbackErr)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch upsert: %w", err)
	}

	s.logger.Debug("batch upsert complete", "count", len(items))

	return nil
}

// --- Path materialization ---

// MaterializePath walks the parent chain to build the full path for an item.
// B-022: Returns empty string (not error) when a parent is not in the DB,
// indicating an orphaned item whose path will be recomputed when the parent
// arrives via a later delta page or CascadePathUpdate.
func (s *SQLiteStore) MaterializePath(ctx context.Context, driveID, itemID string) (string, error) {
	s.logger.Debug("materializing path", "drive_id", driveID, "item_id", itemID)

	segments := s.walkParentChain(ctx, driveID, itemID)

	// nil segments means orphaned parent (B-022).
	if segments == nil {
		return "", nil
	}

	// Reverse to build top-down path.
	reverseStrings(segments)

	path := filepath.Join(segments...)
	s.logger.Debug("materialized path",
		"drive_id", driveID, "item_id", itemID, "path", path)

	return path, nil
}

// walkParentChain collects name segments from item up to root.
// Returns nil (not empty slice) when a parent is missing (B-022 orphan).
func (s *SQLiteStore) walkParentChain(ctx context.Context, driveID, itemID string) []string {
	var segments []string
	currentDriveID := driveID
	currentItemID := itemID

	for {
		item, err := s.GetItem(ctx, currentDriveID, currentItemID)
		if err != nil || item == nil {
			// B-022: parent not found — signal orphan with nil.
			s.logger.Debug("parent not found during path walk",
				"drive_id", currentDriveID, "item_id", currentItemID)
			return nil
		}

		// Root items terminate the walk — they have no name in the path.
		if item.ItemType == ItemTypeRoot {
			break
		}

		segments = append(segments, item.Name)

		if item.ParentID == "" {
			break
		}

		currentDriveID = item.ParentDriveID
		currentItemID = item.ParentID
	}

	return segments
}

// reverseStrings reverses a string slice in-place.
func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// CascadePathUpdate updates all item paths matching an old prefix to use
// a new prefix. Used after a folder rename/move to update all descendant
// paths atomically.
func (s *SQLiteStore) CascadePathUpdate(ctx context.Context, oldPrefix, newPrefix string) error {
	s.logger.Info("cascading path update",
		"old_prefix", oldPrefix, "new_prefix", newPrefix)

	// SUBSTR is 1-based in SQLite, so add 1 to the old prefix length.
	query := `UPDATE items SET path = ? || SUBSTR(path, ?), updated_at = ?
		WHERE path LIKE ? AND is_deleted = 0`

	oldLen := len(oldPrefix) + 1
	pattern := oldPrefix + "%"
	now := NowNano()

	result, err := s.db.ExecContext(ctx, query, newPrefix, oldLen, now, pattern)
	if err != nil {
		return fmt.Errorf("cascade path update %q -> %q: %w",
			oldPrefix, newPrefix, err)
	}

	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		s.logger.Warn("could not read rows affected", "error", rowsErr)
	}

	s.logger.Info("cascade path update complete",
		"old_prefix", oldPrefix, "new_prefix", newPrefix, "affected", affected)

	return nil
}

// tombstoneRetentionHoursPerDay converts days to hours for duration calculation.
const tombstoneRetentionHoursPerDay = 24

// CleanupTombstones removes deleted items older than the retention period.
// Returns the number of rows deleted.
func (s *SQLiteStore) CleanupTombstones(ctx context.Context, retentionDays int) (int64, error) {
	s.logger.Info("cleaning up tombstones", "retention_days", retentionDays)

	cutoff := time.Now().Add(
		-time.Duration(retentionDays) * tombstoneRetentionHoursPerDay * time.Hour,
	).UnixNano()

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM items WHERE is_deleted = 1 AND deleted_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup tombstones: %w", err)
	}

	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		s.logger.Warn("could not read rows affected", "error", rowsErr)
	}

	s.logger.Info("tombstone cleanup complete", "deleted", affected)

	return affected, nil
}

// --- Delta token methods ---

// GetDeltaToken retrieves the stored delta token for a drive.
// Returns empty string if no token exists.
func (s *SQLiteStore) GetDeltaToken(ctx context.Context, driveID string) (string, error) {
	s.logger.Debug("getting delta token", "drive_id", driveID)

	var token string

	err := s.deltaStmts.getToken.QueryRowContext(ctx, driveID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("get delta token %s: %w", driveID, err)
	}

	return token, nil
}

// SaveDeltaToken persists a delta token for a drive (insert or update).
func (s *SQLiteStore) SaveDeltaToken(ctx context.Context, driveID, token string) error {
	s.logger.Debug("saving delta token", "drive_id", driveID)

	_, err := s.deltaStmts.saveToken.ExecContext(ctx, driveID, token, NowNano())
	if err != nil {
		return fmt.Errorf("save delta token %s: %w", driveID, err)
	}

	return nil
}

// DeleteDeltaToken removes the delta token for a drive (e.g., on HTTP 410).
func (s *SQLiteStore) DeleteDeltaToken(ctx context.Context, driveID string) error {
	s.logger.Debug("deleting delta token", "drive_id", driveID)

	_, err := s.deltaStmts.deleteToken.ExecContext(ctx, driveID)
	if err != nil {
		return fmt.Errorf("delete delta token %s: %w", driveID, err)
	}

	return nil
}

// SetDeltaComplete marks whether the initial delta enumeration is complete.
func (s *SQLiteStore) SetDeltaComplete(ctx context.Context, driveID string, complete bool) error {
	s.logger.Debug("setting delta complete", "drive_id", driveID, "complete", complete)

	val := 0
	if complete {
		val = 1
	}

	_, err := s.deltaStmts.setComplete.ExecContext(ctx, driveID, val)
	if err != nil {
		return fmt.Errorf("set delta complete %s: %w", driveID, err)
	}

	return nil
}

// IsDeltaComplete checks whether the initial delta enumeration has completed.
// Returns false if no record exists.
func (s *SQLiteStore) IsDeltaComplete(ctx context.Context, driveID string) (bool, error) {
	s.logger.Debug("checking delta complete", "drive_id", driveID)

	var complete int

	err := s.deltaStmts.isComplete.QueryRowContext(ctx, driveID).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("is delta complete %s: %w", driveID, err)
	}

	return complete == 1, nil
}

// --- Conflict methods ---

// RecordConflict inserts a new conflict record into the ledger.
func (s *SQLiteStore) RecordConflict(ctx context.Context, record *ConflictRecord) error {
	s.logger.Info("recording conflict", "id", record.ID, "path", record.Path)

	_, err := s.conflictStmts.record.ExecContext(ctx,
		record.ID, record.DriveID, record.ItemID, record.Path,
		record.DetectedAt, record.LocalHash, record.RemoteHash,
		record.LocalMtime, record.RemoteMtime,
		string(record.Resolution), record.History,
	)
	if err != nil {
		return fmt.Errorf("record conflict %s: %w", record.ID, err)
	}

	return nil
}

// ListConflicts returns all conflict records for a drive.
func (s *SQLiteStore) ListConflicts(ctx context.Context, driveID string) ([]*ConflictRecord, error) {
	s.logger.Debug("listing conflicts", "drive_id", driveID)

	rows, err := s.conflictStmts.list.QueryContext(ctx, driveID)
	if err != nil {
		return nil, fmt.Errorf("list conflicts %s: %w", driveID, err)
	}
	defer rows.Close()

	return scanConflictRows(rows)
}

// scanConflictRows iterates over sql.Rows and collects ConflictRecords.
func scanConflictRows(rows *sql.Rows) ([]*ConflictRecord, error) {
	var records []*ConflictRecord

	for rows.Next() {
		r := &ConflictRecord{}

		var resolution string

		var resolvedBy *string

		err := rows.Scan(
			&r.ID, &r.DriveID, &r.ItemID, &r.Path, &r.DetectedAt,
			&r.LocalHash, &r.RemoteHash, &r.LocalMtime, &r.RemoteMtime,
			&resolution, &r.ResolvedAt, &resolvedBy, &r.History,
		)
		if err != nil {
			return nil, fmt.Errorf("scan conflict row: %w", err)
		}

		r.Resolution = ConflictResolution(resolution)
		if resolvedBy != nil {
			rb := ConflictResolvedBy(*resolvedBy)
			r.ResolvedBy = &rb
		}

		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflict rows: %w", err)
	}

	return records, nil
}

// ResolveConflict updates a conflict with a resolution and resolver.
func (s *SQLiteStore) ResolveConflict(
	ctx context.Context,
	id string,
	resolution ConflictResolution,
	resolvedBy ConflictResolvedBy,
) error {
	s.logger.Info("resolving conflict",
		"id", id, "resolution", resolution, "resolved_by", resolvedBy)

	now := NowNano()

	_, err := s.conflictStmts.resolve.ExecContext(ctx,
		string(resolution), now, string(resolvedBy), id)
	if err != nil {
		return fmt.Errorf("resolve conflict %s: %w", id, err)
	}

	return nil
}

// ConflictCount returns the number of unresolved conflicts for a drive.
func (s *SQLiteStore) ConflictCount(ctx context.Context, driveID string) (int, error) {
	s.logger.Debug("counting conflicts", "drive_id", driveID)

	var count int

	err := s.conflictStmts.count.QueryRowContext(ctx, driveID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("conflict count %s: %w", driveID, err)
	}

	return count, nil
}

// --- Stale file methods ---

// RecordStaleFile inserts a stale file record.
func (s *SQLiteStore) RecordStaleFile(ctx context.Context, record *StaleRecord) error {
	s.logger.Info("recording stale file",
		"id", record.ID, "path", record.Path, "reason", record.Reason)

	_, err := s.staleStmts.record.ExecContext(ctx,
		record.ID, record.Path, record.Reason, record.DetectedAt, record.Size,
	)
	if err != nil {
		return fmt.Errorf("record stale file %s: %w", record.ID, err)
	}

	return nil
}

// ListStaleFiles returns all stale file records.
func (s *SQLiteStore) ListStaleFiles(ctx context.Context) ([]*StaleRecord, error) {
	s.logger.Debug("listing stale files")

	rows, err := s.staleStmts.list.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stale files: %w", err)
	}
	defer rows.Close()

	var records []*StaleRecord

	for rows.Next() {
		r := &StaleRecord{}
		if err := rows.Scan(&r.ID, &r.Path, &r.Reason, &r.DetectedAt, &r.Size); err != nil {
			return nil, fmt.Errorf("scan stale file row: %w", err)
		}

		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale file rows: %w", err)
	}

	return records, nil
}

// RemoveStaleFile deletes a stale file record by ID.
func (s *SQLiteStore) RemoveStaleFile(ctx context.Context, id string) error {
	s.logger.Debug("removing stale file", "id", id)

	_, err := s.staleStmts.remove.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("remove stale file %s: %w", id, err)
	}

	return nil
}

// --- Upload session methods ---

// SaveUploadSession persists an upload session for crash recovery.
func (s *SQLiteStore) SaveUploadSession(ctx context.Context, record *UploadSessionRecord) error {
	s.logger.Debug("saving upload session",
		"id", record.ID, "local_path", record.LocalPath)

	_, err := s.uploadStmts.save.ExecContext(ctx,
		record.ID, record.DriveID, record.ItemID, record.LocalPath,
		record.SessionURL, record.Expiry, record.BytesUploaded,
		record.TotalSize, record.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("save upload session %s: %w", record.ID, err)
	}

	return nil
}

// GetUploadSession retrieves a single upload session by ID.
// Returns (nil, nil) if no session exists — callers use the nil session
// to distinguish "no resumable upload" from "found session".
func (s *SQLiteStore) GetUploadSession(ctx context.Context, id string) (*UploadSessionRecord, error) {
	s.logger.Debug("getting upload session", "id", id)

	r := &UploadSessionRecord{}

	err := s.uploadStmts.get.QueryRowContext(ctx, id).Scan(
		&r.ID, &r.DriveID, &r.ItemID, &r.LocalPath,
		&r.SessionURL, &r.Expiry, &r.BytesUploaded,
		&r.TotalSize, &r.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // nil session means "not found", matching GetItem/GetItemByPath pattern
	}

	if err != nil {
		return nil, fmt.Errorf("get upload session %s: %w", id, err)
	}

	return r, nil
}

// DeleteUploadSession removes an upload session record.
func (s *SQLiteStore) DeleteUploadSession(ctx context.Context, id string) error {
	s.logger.Debug("deleting upload session", "id", id)

	_, err := s.uploadStmts.delete.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("delete upload session %s: %w", id, err)
	}

	return nil
}

// ListExpiredSessions returns all upload sessions that have expired.
func (s *SQLiteStore) ListExpiredSessions(ctx context.Context, now int64) ([]*UploadSessionRecord, error) {
	s.logger.Debug("listing expired upload sessions")

	rows, err := s.uploadStmts.listExpired.QueryContext(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("list expired sessions: %w", err)
	}
	defer rows.Close()

	var records []*UploadSessionRecord

	for rows.Next() {
		r := &UploadSessionRecord{}

		err := rows.Scan(
			&r.ID, &r.DriveID, &r.ItemID, &r.LocalPath,
			&r.SessionURL, &r.Expiry, &r.BytesUploaded,
			&r.TotalSize, &r.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan upload session row: %w", err)
		}

		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upload session rows: %w", err)
	}

	return records, nil
}

// --- Config snapshot methods ---

// GetConfigSnapshot retrieves a config snapshot value by key.
// Returns empty string if the key doesn't exist.
func (s *SQLiteStore) GetConfigSnapshot(ctx context.Context, key string) (string, error) {
	s.logger.Debug("getting config snapshot", "key", key)

	var value string

	err := s.configStmts.get.QueryRowContext(ctx, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("get config snapshot %q: %w", key, err)
	}

	return value, nil
}

// SaveConfigSnapshot persists a config snapshot key-value pair.
func (s *SQLiteStore) SaveConfigSnapshot(ctx context.Context, key, value string) error {
	s.logger.Debug("saving config snapshot", "key", key)

	_, err := s.configStmts.save.ExecContext(ctx, key, value)
	if err != nil {
		return fmt.Errorf("save config snapshot %q: %w", key, err)
	}

	return nil
}

// --- Maintenance methods ---

// Checkpoint forces a WAL checkpoint to consolidate the WAL file into the
// main database.
func (s *SQLiteStore) Checkpoint() error {
	s.logger.Debug("running WAL checkpoint")

	_, err := s.db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}

	return nil
}

// Close closes all prepared statements and the database connection.
func (s *SQLiteStore) Close() error {
	s.logger.Info("closing sync state database")

	if err := s.closeStatements(); err != nil {
		s.logger.Error("error closing statements", "error", err)
	}

	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}

	return nil
}

// closeStatements closes all prepared statements, collecting errors.
func (s *SQLiteStore) closeStatements() error {
	stmts := []*sql.Stmt{
		s.itemStmts.get, s.itemStmts.upsert, s.itemStmts.markDeleted,
		s.itemStmts.deleteByKey, s.itemStmts.listChildren, s.itemStmts.getByPath,
		s.itemStmts.listAllActive, s.itemStmts.listSynced,
		s.deltaStmts.getToken, s.deltaStmts.saveToken,
		s.deltaStmts.deleteToken, s.deltaStmts.setComplete,
		s.deltaStmts.isComplete,
		s.conflictStmts.record, s.conflictStmts.list,
		s.conflictStmts.resolve, s.conflictStmts.count,
		s.staleStmts.record, s.staleStmts.list, s.staleStmts.remove,
		s.uploadStmts.save, s.uploadStmts.get,
		s.uploadStmts.delete, s.uploadStmts.listExpired,
		s.configStmts.get, s.configStmts.save,
	}

	var errs []string

	for _, stmt := range stmts {
		if stmt != nil {
			if err := stmt.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("close statements: %s", strings.Join(errs, "; "))
	}

	return nil
}

// Compile-time interface check.
var _ Store = (*SQLiteStore)(nil)
