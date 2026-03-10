package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	// Pure-Go SQLite driver (no CGO).
	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT drive_id, item_id, path, parent_id, item_type,
		local_hash, remote_hash, size, mtime, synced_at, etag
		FROM baseline`

	//nolint:gosec // G101: "token" is a delta cursor, not credentials.
	sqlGetDeltaToken = `SELECT token FROM delta_tokens WHERE drive_id = ? AND scope_id = ?`

	sqlUpsertBaseline = `INSERT INTO baseline
		(drive_id, item_id, path, parent_id, item_type, local_hash, remote_hash,
		 size, mtime, synced_at, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, item_id) DO UPDATE SET
		 path = excluded.path,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 size = excluded.size,
		 mtime = excluded.mtime,
		 synced_at = excluded.synced_at,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`

	// Shared column list for all sync_failures SELECT queries. Must match
	// the scan order in scanSyncFailureRows. Update both when adding columns.
	sqlSelectSyncFailureCols = `path, drive_id, direction, category,
		COALESCE(issue_type, ''), COALESCE(item_id, ''),
		failure_count, COALESCE(next_retry_at, 0),
		COALESCE(last_error, ''), COALESCE(http_status, 0),
		first_seen_at, last_seen_at,
		COALESCE(file_size, 0), COALESCE(local_hash, ''),
		scope_key`

	sqlInsertConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime,
		 resolution, resolved_at, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpsertDeltaToken = `INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, token, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, scope_id) DO UPDATE SET
		 scope_drive = excluded.scope_drive,
		 token = excluded.token,
		 updated_at = excluded.updated_at`

	sqlListConflicts = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE resolution = 'unresolved'
		ORDER BY detected_at`

	sqlListAllConflicts = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts
		ORDER BY detected_at DESC`

	sqlGetConflictByID = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE id = ?`

	sqlGetConflictByPath = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE path = ? AND resolution = 'unresolved'
		ORDER BY detected_at DESC LIMIT 1`

	sqlResolveConflict = `UPDATE conflicts
		SET resolution = ?, resolved_at = ?, resolved_by = 'user'
		WHERE id = ? AND resolution = 'unresolved'`
)

// SQL statements for remote_state operations.
const (
	sqlGetRemoteStateRow = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, sync_status, observed_at
		FROM remote_state WHERE drive_id = ? AND item_id = ?`

	sqlInsertRemoteState = `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
		 sync_status, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpdateRemoteState = `UPDATE remote_state SET
		path = ?, parent_id = ?, item_type = ?, hash = ?, size = ?, mtime = ?, etag = ?,
		previous_path = ?, sync_status = ?, observed_at = ?
		WHERE drive_id = ? AND item_id = ?`
)

// Compile-time interface satisfaction checks.
var (
	_ ObservationWriter   = (*SyncStore)(nil)
	_ OutcomeWriter       = (*SyncStore)(nil)
	_ StateReader         = (*SyncStore)(nil)
	_ StateAdmin          = (*SyncStore)(nil)
	_ SyncFailureRecorder = (*SyncStore)(nil)
)

// SyncStore is the sole writer to the sync database. It loads the
// baseline at pass start and commits outcomes at pass end.
type SyncStore struct {
	db       *sql.DB
	baseline *Baseline
	logger   *slog.Logger
	nowFunc  func() time.Time // injectable for deterministic tests
}

// NewSyncStore opens the SQLite database at dbPath, runs migrations,
// and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewSyncStore(dbPath string, logger *slog.Logger) (*SyncStore, error) {
	// DSN parameters ensure pragmas apply to every connection from the pool.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"+
			"&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"+
			"&_pragma=journal_size_limit(67108864)",
		dbPath,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sync: opening database %s: %w", dbPath, err)
	}

	// Sole-writer pattern: only one connection writes at a time.
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	if err := runMigrations(ctx, db, logger); err != nil {
		db.Close()
		return nil, err
	}

	logger.Info("baseline manager initialized", slog.String("db_path", dbPath))

	return &SyncStore{
		db:      db,
		logger:  logger,
		nowFunc: time.Now,
	}, nil
}

// Load reads the entire baseline table into memory, populating ByPath and
// ByID maps. The result is cached on the manager — subsequent calls return
// the cached baseline without querying the database. The cache is kept
// consistent by CommitOutcome(), which incrementally patches the in-memory
// maps via updateBaselineCache() after each transaction. This is safe
// because SyncStore exclusively owns the database (sole-writer
// pattern with SetMaxOpenConns(1)).
func (m *SyncStore) Load(ctx context.Context) (*Baseline, error) {
	if m.baseline != nil {
		return m.baseline, nil
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}
	defer rows.Close()

	b := &Baseline{
		byPath: make(map[string]*BaselineEntry),
		byID:   make(map[driveid.ItemKey]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows)
		if err != nil {
			return nil, err
		}

		b.byPath[entry.Path] = entry
		b.byID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating baseline rows: %w", err)
	}

	m.baseline = b
	m.logger.Debug("baseline loaded", slog.Int("entries", len(b.byPath)))

	return b, nil
}

// scanBaselineRow scans a single row from the baseline table, handling
// nullable columns with sql.Null* types.
func scanBaselineRow(rows *sql.Rows) (*BaselineEntry, error) {
	var (
		e          BaselineEntry
		itemType   string
		parentID   sql.NullString
		localHash  sql.NullString
		remoteHash sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := rows.Scan(
		&e.DriveID, &e.ItemID, &e.Path, &parentID, &itemType,
		&localHash, &remoteHash, &size, &mtime, &e.SyncedAt, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning baseline row: %w", err)
	}

	parsed, err := ParseItemType(itemType)
	if err != nil {
		return nil, err
	}

	e.ItemType = parsed
	e.ParentID = parentID.String
	e.LocalHash = localHash.String
	e.RemoteHash = remoteHash.String
	e.ETag = etag.String

	if size.Valid {
		e.Size = size.Int64
	}

	if mtime.Valid {
		e.Mtime = mtime.Int64
	}

	return &e, nil
}

// GetDeltaToken returns the saved delta token for a drive and scope, or empty
// string if no token has been saved yet. Use scopeID="" for the primary
// drive-level delta; use a remoteItem.id for shortcut-scoped deltas.
func (m *SyncStore) GetDeltaToken(ctx context.Context, driveID, scopeID string) (string, error) {
	var token string

	err := m.db.QueryRowContext(ctx, sqlGetDeltaToken, driveID, scopeID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: getting delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return token, nil
}

// CommitOutcome atomically applies a single outcome to the baseline in a
// SQLite transaction. After the DB write, the in-memory baseline cache is
// updated incrementally (Put or Delete).
func (m *SyncStore) CommitOutcome(ctx context.Context, outcome *Outcome) error {
	if !outcome.Success {
		return nil
	}

	// Ensure baseline is loaded so we can update the in-memory cache.
	if m.baseline == nil {
		if _, loadErr := m.Load(ctx); loadErr != nil {
			return fmt.Errorf("sync: loading baseline before commit outcome: %w", loadErr)
		}
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning commit outcome transaction: %w", err)
	}
	defer tx.Rollback()

	syncedAt := m.nowFunc().UnixNano()

	if applyErr := applySingleOutcome(ctx, tx, outcome, syncedAt); applyErr != nil {
		return applyErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("sync: committing outcome transaction: %w", commitErr)
	}

	// Update in-memory baseline cache incrementally.
	m.updateBaselineCache(outcome, syncedAt)

	return nil
}

// applySingleOutcome dispatches a single outcome to the appropriate DB helper.
func applySingleOutcome(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	var err error

	switch o.Action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		err = commitUpsert(ctx, tx, o, syncedAt)
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		err = commitDelete(ctx, tx, o.Path)
	case ActionLocalMove, ActionRemoteMove:
		err = commitMove(ctx, tx, o, syncedAt)
	case ActionConflict:
		err = commitConflict(ctx, tx, o, syncedAt)
	}

	if err != nil {
		return err
	}

	// Update remote_state in the same transaction.
	return updateRemoteStateOnOutcome(ctx, tx, o)
}

// updateBaselineCache applies a single outcome to the in-memory baseline,
// keeping the cache consistent without a full DB reload.
func (m *SyncStore) updateBaselineCache(o *Outcome, syncedAt int64) {
	switch o.Action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		m.baseline.Put(outcomeToEntry(o, syncedAt))
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		m.baseline.Delete(o.Path)
	case ActionLocalMove, ActionRemoteMove:
		m.baseline.Delete(o.OldPath)
		m.baseline.Put(outcomeToEntry(o, syncedAt))
	case ActionConflict:
		if o.ResolvedBy == ResolvedByAuto {
			m.baseline.Put(outcomeToEntry(o, syncedAt))
		} else if o.ConflictType == ConflictEditDelete {
			// Unresolved edit-delete conflict from local delete: the original file
			// is gone (renamed to conflict copy), so remove the baseline entry.
			m.baseline.Delete(o.Path)
		}
	}
}

// outcomeToEntry converts an Outcome into a BaselineEntry for cache update.
func outcomeToEntry(o *Outcome, syncedAt int64) *BaselineEntry {
	return &BaselineEntry{
		Path:       o.Path,
		DriveID:    o.DriveID,
		ItemID:     o.ItemID,
		ParentID:   o.ParentID,
		ItemType:   o.ItemType,
		LocalHash:  o.LocalHash,
		RemoteHash: o.RemoteHash,
		Size:       o.Size,
		Mtime:      o.Mtime,
		SyncedAt:   syncedAt,
		ETag:       o.ETag,
	}
}

// CommitDeltaToken persists a delta token in its own transaction, separate
// from baseline updates. Used after all actions in a pass complete.
// Use scopeID="" and scopeDrive=driveID for the primary drive-level delta.
// For shortcut-scoped deltas, scopeID=remoteItem.id and scopeDrive=remoteItem.driveId.
func (m *SyncStore) CommitDeltaToken(ctx context.Context, token, driveID, scopeID, scopeDrive string) error {
	if token == "" {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning delta token transaction: %w", err)
	}
	defer tx.Rollback()

	updatedAt := m.nowFunc().UnixNano()
	if saveErr := m.saveDeltaToken(ctx, tx, driveID, scopeID, scopeDrive, token, updatedAt); saveErr != nil {
		return saveErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("sync: committing delta token transaction: %w", commitErr)
	}

	m.logger.Debug("delta token committed",
		slog.String("drive_id", driveID),
		slog.String("scope_id", scopeID),
	)

	return nil
}

// DeleteDeltaToken removes a delta token for a specific drive and scope.
// Used when a shortcut is removed to clean up its scoped delta token.
func (m *SyncStore) DeleteDeltaToken(ctx context.Context, driveID, scopeID string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM delta_tokens WHERE drive_id = ? AND scope_id = ?`,
		driveID, scopeID)
	if err != nil {
		return fmt.Errorf("sync: deleting delta token for drive %s scope %s: %w", driveID, scopeID, err)
	}

	return nil
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes. Handles the case where a
// server-side delete+recreate assigns a new item_id for an existing path
// by removing the stale row first (prevents UNIQUE constraint violation on path).
func commitUpsert(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	// Remove any stale baseline row at the same path but different identity.
	// This happens when the server assigns a new item_id for a path that
	// was previously tracked under a different ID (delete+recreate, or
	// re-upload after server-side deletion).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM baseline WHERE path = ? AND NOT (drive_id = ? AND item_id = ?)`,
		o.Path, o.DriveID, o.ItemID,
	); err != nil {
		return fmt.Errorf("sync: clearing stale baseline for %s: %w", o.Path, err)
	}

	_, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		o.DriveID, o.ItemID, o.Path,
		nullString(o.ParentID),
		o.ItemType.String(),
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Size),
		nullInt64(o.Mtime),
		syncedAt,
		nullString(o.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: upserting baseline for %s: %w", o.Path, err)
	}

	return nil
}

// commitDelete removes a baseline entry for delete and cleanup outcomes.
func commitDelete(ctx context.Context, tx *sql.Tx, path string) error {
	_, err := tx.ExecContext(ctx, sqlDeleteBaseline, path)
	if err != nil {
		return fmt.Errorf("sync: deleting baseline for %s: %w", path, err)
	}

	return nil
}

// commitMove atomically updates the path for move outcomes. With the ID-based
// PK, a move is a single UPDATE (not DELETE+INSERT) — the row identity
// (drive_id, item_id) doesn't change, only the path does.
func commitMove(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	// Upsert handles both the path update and all other field updates.
	// The ON CONFLICT(drive_id, item_id) clause matches the existing row
	// and updates path + all mutable fields atomically.
	return commitUpsert(ctx, tx, o, syncedAt)
}

// commitConflict inserts a conflict record. Auto-resolved conflicts
// (Outcome.ResolvedBy == ResolvedByAuto) are inserted as already resolved, and
// the baseline is updated (the upload created a new remote item).
func commitConflict(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	conflictID := uuid.New().String()

	resolution := ResolutionUnresolved
	var resolvedAt sql.NullInt64
	var resolvedBy sql.NullString

	if o.ResolvedBy == ResolvedByAuto {
		resolution = ResolutionKeepLocal
		resolvedAt = sql.NullInt64{Int64: syncedAt, Valid: true}
		resolvedBy = sql.NullString{String: ResolvedByAuto, Valid: true}
	}

	_, err := tx.ExecContext(ctx, sqlInsertConflict,
		conflictID, o.DriveID,
		nullString(o.ItemID),
		o.Path, o.ConflictType, syncedAt,
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Mtime),
		nullInt64(o.RemoteMtime),
		resolution, resolvedAt, resolvedBy,
	)
	if err != nil {
		return fmt.Errorf("sync: inserting conflict for %s: %w", o.Path, err)
	}

	// Auto-resolved conflicts also update the baseline (the upload created
	// a new remote item that should be tracked).
	if o.ResolvedBy == ResolvedByAuto {
		if upsertErr := commitUpsert(ctx, tx, o, syncedAt); upsertErr != nil {
			return upsertErr
		}
	}

	// Unresolved edit-delete conflict from local delete: the original file is
	// gone (renamed to conflict copy), so remove the baseline entry (B-133).
	if o.ResolvedBy == "" && o.ConflictType == ConflictEditDelete {
		if delErr := commitDelete(ctx, tx, o.Path); delErr != nil {
			return delErr
		}
	}

	return nil
}

// updateRemoteStateOnOutcome updates remote_state based on a completed action
// outcome, called from within the same transaction as the baseline update.
// Silently skips if no matching remote_state row exists (e.g., upload-only mode).
func updateRemoteStateOnOutcome(ctx context.Context, tx *sql.Tx, o *Outcome) error {
	if o.ItemID == "" || o.DriveID.IsZero() {
		return nil
	}

	switch o.Action { //nolint:exhaustive // only action types that touch remote_state
	case ActionDownload:
		// Hash guard: only transition if the remote_state hash matches the
		// downloaded hash. Prevents stale overwrite when a new observation
		// arrived while the download was in progress.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status = ? AND hash IS ?`,
			statusSynced,
			o.DriveID.String(), o.ItemID, statusDownloading, nullString(o.RemoteHash),
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for download %s: %w", o.Path, err)
		}

		// Clear any prior sync_failures for this path on download success.
		if _, delErr := tx.ExecContext(ctx,
			`DELETE FROM sync_failures WHERE path = ?`, o.Path,
		); delErr != nil {
			return fmt.Errorf("sync: clearing sync failure on download success for %s: %w", o.Path, delErr)
		}

	case ActionLocalDelete:
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status = ?`,
			statusDeleted,
			o.DriveID.String(), o.ItemID, statusDeleting,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for local delete %s: %w", o.Path, err)
		}

		// Clear any prior sync_failures for this path on delete success.
		if _, delErr := tx.ExecContext(ctx,
			`DELETE FROM sync_failures WHERE path = ?`, o.Path,
		); delErr != nil {
			return fmt.Errorf("sync: clearing sync failure on delete success for %s: %w", o.Path, delErr)
		}

	case ActionUpload, ActionFolderCreate:
		// Unconditional: upload resolves any state.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?, hash = ?, size = ?, mtime = ?
			WHERE drive_id = ? AND item_id = ?`,
			statusSynced, nullString(o.RemoteHash), nullInt64(o.Size), nullInt64(o.Mtime),
			o.DriveID.String(), o.ItemID,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for upload %s: %w", o.Path, err)
		}

		// Clear any prior sync_failures for this path on upload success. Uses
		// path-only match because the failure may have been recorded with an
		// empty drive_id (CLI, pre-migration data) while the outcome has a
		// real drive_id. A successful upload resolves all prior failures.
		if _, delErr := tx.ExecContext(ctx,
			`DELETE FROM sync_failures WHERE path = ?`, o.Path,
		); delErr != nil {
			return fmt.Errorf("sync: clearing sync failure on upload success for %s: %w", o.Path, delErr)
		}

	case ActionLocalMove, ActionRemoteMove:
		// Move success: update path and mark synced.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET path = ?, sync_status = ?
			WHERE drive_id = ? AND item_id = ?`,
			o.Path, statusSynced,
			o.DriveID.String(), o.ItemID,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for move %s: %w", o.Path, err)
		}

		// Clear any prior sync_failures for this path on move success.
		if _, delErr := tx.ExecContext(ctx,
			`DELETE FROM sync_failures WHERE path = ?`, o.Path,
		); delErr != nil {
			return fmt.Errorf("sync: clearing sync failure on move success for %s: %w", o.Path, delErr)
		}
	}

	return nil
}

// saveDeltaToken persists the delta token in the same transaction as
// baseline updates.
func (m *SyncStore) saveDeltaToken(
	ctx context.Context, tx *sql.Tx, driveID, scopeID, scopeDrive, token string, updatedAt int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpsertDeltaToken, driveID, scopeID, scopeDrive, token, updatedAt)
	if err != nil {
		return fmt.Errorf("sync: saving delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return nil
}

// ListConflicts returns all unresolved conflicts ordered by detection time.
func (m *SyncStore) ListConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListConflicts)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) ordered
// by detection time descending. Used by 'conflicts --history'.
func (m *SyncStore) ListAllConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListAllConflicts)
}

// queryConflicts executes a conflict query and scans the results.
func (m *SyncStore) queryConflicts(ctx context.Context, query string) ([]ConflictRecord, error) {
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []ConflictRecord

	for rows.Next() {
		c, err := scanConflictRow(rows)
		if err != nil {
			return nil, err
		}

		conflicts = append(conflicts, *c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating conflict rows: %w", err)
	}

	return conflicts, nil
}

// GetConflict looks up a conflict by UUID or path. Tries ID first, falls
// back to path (most recent unresolved conflict for that path).
func (m *SyncStore) GetConflict(ctx context.Context, idOrPath string) (*ConflictRecord, error) {
	// Try by ID first.
	row := m.db.QueryRowContext(ctx, sqlGetConflictByID, idOrPath)

	c, err := scanConflictRowSingle(row)
	if err == nil {
		return c, nil
	}

	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("sync: getting conflict by ID %q: %w", idOrPath, err)
	}

	// Fall back to path lookup.
	row = m.db.QueryRowContext(ctx, sqlGetConflictByPath, idOrPath)

	c, err = scanConflictRowSingle(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("sync: conflict not found for %q", idOrPath)
	}

	if err != nil {
		return nil, fmt.Errorf("sync: getting conflict by path %q: %w", idOrPath, err)
	}

	return c, nil
}

// ResolveConflict marks a conflict as resolved with the given resolution
// strategy. Only updates unresolved conflicts (idempotent-safe).
func (m *SyncStore) ResolveConflict(ctx context.Context, id, resolution string) error {
	resolvedAt := m.nowFunc().UnixNano()

	result, err := m.db.ExecContext(ctx, sqlResolveConflict, resolution, resolvedAt, id)
	if err != nil {
		return fmt.Errorf("sync: resolving conflict %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sync: checking rows affected for conflict %s: %w", id, err)
	}

	if rows == 0 {
		return fmt.Errorf("sync: conflict %s not found or already resolved", id)
	}

	m.logger.Info("conflict resolved",
		slog.String("id", id),
		slog.String("resolution", resolution),
	)

	return nil
}

// CheckCacheConsistency reloads baseline entries from the database and compares
// them with the in-memory cache. Returns the number of mismatches found (report-only,
// no auto-fix). Intended for periodic verification in watch mode (B-198).
func (m *SyncStore) CheckCacheConsistency(ctx context.Context) (int, error) {
	if m.baseline == nil {
		return 0, nil
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return 0, fmt.Errorf("sync: querying baseline for consistency check: %w", err)
	}
	defer rows.Close()

	dbEntries := make(map[string]*BaselineEntry)

	for rows.Next() {
		entry, scanErr := scanBaselineRow(rows)
		if scanErr != nil {
			return 0, scanErr
		}

		dbEntries[entry.Path] = entry
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sync: iterating baseline rows for consistency check: %w", err)
	}

	mismatches := 0

	// Check for entries in cache not in DB, or with different values.
	for p, cached := range m.baseline.byPath {
		dbEntry, ok := dbEntries[p]
		if !ok {
			m.logger.Warn("cache consistency: entry in cache not in DB",
				slog.String("path", p),
			)

			mismatches++

			continue
		}

		if cached.LocalHash != dbEntry.LocalHash || cached.RemoteHash != dbEntry.RemoteHash ||
			cached.Size != dbEntry.Size || cached.ItemID != dbEntry.ItemID {
			m.logger.Warn("cache consistency: field mismatch",
				slog.String("path", p),
			)

			mismatches++
		}
	}

	// Check for entries in DB not in cache.
	for p := range dbEntries {
		if _, ok := m.baseline.byPath[p]; !ok {
			m.logger.Warn("cache consistency: entry in DB not in cache",
				slog.String("path", p),
			)

			mismatches++
		}
	}

	if mismatches > 0 {
		m.logger.Warn("cache consistency check complete",
			slog.Int("mismatches", mismatches),
		)
	}

	return mismatches, nil
}

// PruneResolvedConflicts deletes resolved conflicts whose detection time is
// older than the given retention duration. Unresolved conflicts are never
// pruned. Returns the number of deleted rows (B-087).
func (m *SyncStore) PruneResolvedConflicts(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := m.nowFunc().Add(-retention).UnixNano()

	result, err := m.db.ExecContext(ctx,
		`DELETE FROM conflicts WHERE resolution != 'unresolved' AND detected_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: pruning resolved conflicts: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: checking pruned conflict count: %w", err)
	}

	if rows > 0 {
		m.logger.Info("pruned resolved conflicts",
			slog.Int64("pruned", rows),
			slog.Duration("retention", retention),
		)
	}

	return int(rows), nil
}

// conflictScanner abstracts the Scan method shared by *sql.Rows and *sql.Row,
// allowing a single scan implementation for both multi-row and single-row
// conflict queries (B-149).
type conflictScanner interface {
	Scan(dest ...any) error
}

// scanConflict scans a single conflict row from any scanner (*sql.Rows or
// *sql.Row), handling nullable columns. The `history` column is intentionally
// excluded — it is dormant/unused (B-160).
func scanConflict(s conflictScanner) (*ConflictRecord, error) {
	var (
		c           ConflictRecord
		itemID      sql.NullString
		localHash   sql.NullString
		remoteHash  sql.NullString
		localMtime  sql.NullInt64
		remoteMtime sql.NullInt64
		resolvedAt  sql.NullInt64
		resolvedBy  sql.NullString
	)

	err := s.Scan(
		&c.ID, &c.DriveID, &itemID, &c.Path, &c.ConflictType,
		&c.DetectedAt, &localHash, &remoteHash, &localMtime, &remoteMtime,
		&c.Resolution, &resolvedAt, &resolvedBy,
	)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with context
	}

	c.ItemID = itemID.String
	c.Name = path.Base(c.Path) // derived for display convenience (B-071)
	c.LocalHash = localHash.String
	c.RemoteHash = remoteHash.String
	c.ResolvedBy = resolvedBy.String

	if localMtime.Valid {
		c.LocalMtime = localMtime.Int64
	}

	if remoteMtime.Valid {
		c.RemoteMtime = remoteMtime.Int64
	}

	if resolvedAt.Valid {
		c.ResolvedAt = resolvedAt.Int64
	}

	return &c, nil
}

// scanConflictRow scans a conflict from a multi-row result set. Delegates
// to scanConflict via the conflictScanner interface (B-149).
func scanConflictRow(rows *sql.Rows) (*ConflictRecord, error) {
	c, err := scanConflict(rows)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict row: %w", err)
	}

	return c, nil
}

// scanConflictRowSingle scans a conflict from a single-row result.
// Returns sql.ErrNoRows transparently for callers that need it (B-149).
func scanConflictRowSingle(row *sql.Row) (*ConflictRecord, error) {
	return scanConflict(row)
}

// rawDB returns the underlying database connection for test access.
// Unexported to prevent external packages from bypassing typed interfaces.
func (m *SyncStore) rawDB() *sql.DB {
	return m.db
}

// Checkpoint performs WAL checkpoint and optionally prunes soft-deleted rows
// older than retention. Called: after initial sync, every 30 minutes, and on
// shutdown. Pass retention=0 to skip pruning (WAL checkpoint only).
func (m *SyncStore) Checkpoint(ctx context.Context, retention time.Duration) error {
	if _, err := m.db.ExecContext(ctx,
		"PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		m.logger.Warn("WAL checkpoint failed", slog.String("error", err.Error()))
	}

	if retention <= 0 {
		return nil
	}

	cutoff := m.nowFunc().Add(-retention).UnixNano()

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM remote_state WHERE sync_status = 'deleted' AND observed_at < ?`,
		cutoff); err != nil {
		return fmt.Errorf("prune deleted remote_state: %w", err)
	}

	// Actionable failures are kept for user visibility but pruned after retention
	// to prevent unbounded growth of stale entries.
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'actionable' AND last_seen_at < ?`,
		cutoff); err != nil {
		return fmt.Errorf("prune actionable sync_failures: %w", err)
	}

	return nil
}

// Close checkpoints the WAL and closes the underlying database connection.
// The explicit checkpoint ensures cross-process readers (e.g., `issues
// --history` after `sync`) see all committed data when they open a new
// connection to the same database file.
func (m *SyncStore) Close() error {
	// WAL checkpoint only (no pruning) on close.
	if err := m.Checkpoint(context.Background(), 0); err != nil {
		m.logger.Warn("checkpoint failed on close", slog.String("error", err.Error()))
	}

	return m.db.Close()
}

// CommitObservation atomically persists observed remote state and advances the
// delta token in a single transaction. Called by the remote observer after each
// successful delta poll.
//
// For each ObservedItem:
//   - New item (no existing row): INSERT with pending_download (skip if deleted)
//   - Existing item: call computeNewStatus() and UPDATE only if changed
//   - Path change: set previous_path for move tracking
func (m *SyncStore) CommitObservation(ctx context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning observation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := m.nowFunc().UnixNano()

	for i := range events {
		if err := m.processObservedItem(ctx, tx, &events[i], now); err != nil {
			return err
		}
	}

	// Persist delta token in the same transaction.
	if newToken != "" {
		if err := m.saveDeltaToken(ctx, tx, driveID.String(), "", driveID.String(), newToken, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}

	m.logger.Debug("observations committed",
		slog.Int("items", len(events)),
		slog.String("drive_id", driveID.String()),
	)

	return nil
}

// processObservedItem handles a single item within the CommitObservation transaction.
func (m *SyncStore) processObservedItem(ctx context.Context, tx *sql.Tx, item *ObservedItem, now int64) error {
	existing := m.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		// No existing row — skip deleted items we've never seen.
		if item.IsDeleted {
			return nil
		}

		return m.insertRemoteState(ctx, tx, item, now)
	}

	// Existing row — compute new status.
	newStatus, changed := computeNewStatus(existing.SyncStatus, existing.Hash, item.Hash, item.IsDeleted)

	// Track path changes for move detection.
	pathChanged := item.Path != "" && item.Path != existing.Path

	// Update if status changed OR path changed (moves with same hash).
	if !changed && !pathChanged {
		return nil
	}

	if !changed {
		newStatus = existing.SyncStatus
	}

	var previousPath string
	if pathChanged {
		previousPath = existing.Path
	}

	return m.updateRemoteStateFromObs(ctx, tx, item, newStatus, previousPath, now)
}

// scanRemoteStateRow reads a single remote_state row within a transaction.
// Returns nil if no row exists.
func (m *SyncStore) scanRemoteStateRow(ctx context.Context, tx *sql.Tx, driveID, itemID string) *RemoteStateRow {
	var (
		row      RemoteStateRow
		parentID sql.NullString
		hash     sql.NullString
		size     sql.NullInt64
		mtime    sql.NullInt64
		etag     sql.NullString
		prevPath sql.NullString
	)

	err := tx.QueryRowContext(ctx, sqlGetRemoteStateRow, driveID, itemID).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt,
	)
	if err != nil {
		return nil
	}

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String

	if size.Valid {
		row.Size = size.Int64
	}

	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row
}

// insertRemoteState inserts a new remote_state row for a newly observed item.
func (m *SyncStore) insertRemoteState(ctx context.Context, tx *sql.Tx, item *ObservedItem, now int64) error {
	_, err := tx.ExecContext(ctx, sqlInsertRemoteState,
		item.DriveID.String(), item.ItemID, item.Path,
		nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullInt64(item.Size), nullInt64(item.Mtime),
		nullString(item.ETag),
		statusPendingDownload, now,
	)
	if err != nil {
		return fmt.Errorf("sync: inserting remote_state for %s: %w", item.Path, err)
	}

	return nil
}

// updateRemoteStateFromObs updates an existing remote_state row with observation data.
func (m *SyncStore) updateRemoteStateFromObs(
	ctx context.Context, tx *sql.Tx, item *ObservedItem,
	newStatus, previousPath string, now int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpdateRemoteState,
		item.Path, nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullInt64(item.Size), nullInt64(item.Mtime),
		nullString(item.ETag),
		nullString(previousPath), newStatus, now,
		item.DriveID.String(), item.ItemID,
	)
	if err != nil {
		return fmt.Errorf("sync: updating remote_state for %s: %w", item.Path, err)
	}

	return nil
}

// RecordFailure is the unified failure recording method. It always runs in a
// transaction and handles all failure types (upload, download, delete).
//
// For download/delete failures, it atomically transitions remote_state
// (downloading→download_failed, deleting→delete_failed). The WHERE clause
// is a natural no-op when no matching row exists (uploads, pre-upload
// validation, permission checks).
//
// When ItemID is not provided and direction is download/delete, it is
// auto-resolved from remote_state within the same transaction.
//
// Actionable issues (determined by isActionableIssue) get category='actionable'
// with no next_retry_at. Transient issues use exponential backoff.
//
// UPSERT ON CONFLICT uses COALESCE for issue_type, item_id, file_size,
// local_hash to preserve existing values when new values are empty/zero.
func (m *SyncStore) RecordFailure(ctx context.Context, p *SyncFailureParams) error {
	now := m.nowFunc()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning failure transaction for %s: %w", p.Path, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: Transition remote_state status for download/delete failures.
	if p.Direction == strDownload || p.Direction == strDelete {
		if transErr := m.transitionRemoteStateOnFailure(ctx, tx, p.Path); transErr != nil {
			return transErr
		}
	}

	// Step 2: Classify failure and compute backoff.
	category, nextRetryNano, currentFailures := m.classifyAndBackoff(ctx, tx, p, now)
	newCount := currentFailures + 1
	nowNano := now.UnixNano()

	// Step 3: Auto-resolve item_id from remote_state for download/delete
	// when caller didn't provide one.
	itemID := p.ItemID
	if itemID == "" && (p.Direction == strDownload || p.Direction == strDelete) {
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT item_id FROM remote_state WHERE path = ? AND drive_id = ?`,
			p.Path, p.DriveID.String(),
		).Scan(&itemID); scanErr != nil && scanErr != sql.ErrNoRows {
			return fmt.Errorf("sync: reading item_id for %s: %w", p.Path, scanErr)
		}
	}

	// Step 5: UPSERT into sync_failures with full field set.
	// COALESCE preserves existing values when new values are empty/zero.
	// NOTE: UpsertActionableFailures has a parallel INSERT with different
	// ON CONFLICT semantics — update both when adding columns.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sync_failures
			(path, drive_id, direction, category, issue_type, item_id,
			 failure_count, next_retry_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, drive_id) DO UPDATE SET
			direction = excluded.direction,
			category = excluded.category,
			issue_type = COALESCE(excluded.issue_type, sync_failures.issue_type),
			item_id = COALESCE(excluded.item_id, sync_failures.item_id),
			failure_count = sync_failures.failure_count + 1,
			next_retry_at = excluded.next_retry_at,
			last_error = excluded.last_error,
			http_status = excluded.http_status,
			last_seen_at = excluded.last_seen_at,
			file_size = COALESCE(excluded.file_size, sync_failures.file_size),
			local_hash = COALESCE(excluded.local_hash, sync_failures.local_hash),
			scope_key = excluded.scope_key`,
		p.Path, p.DriveID.String(), p.Direction, category,
		nullString(p.IssueType), nullString(itemID),
		newCount, nextRetryNano, p.ErrMsg, p.HTTPStatus,
		nowNano, nowNano, nullInt64(p.FileSize), nullString(p.LocalHash), p.ScopeKey,
	)
	if err != nil {
		return fmt.Errorf("sync: recording sync failure for %s: %w", p.Path, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing failure for %s: %w", p.Path, err)
	}

	return nil
}

// classifyAndBackoff reads the current failure count, determines whether
// the issue is actionable or transient, and computes the next retry time
// for transient issues. Returns (category, nextRetryNano, currentFailures).
func (m *SyncStore) classifyAndBackoff(
	ctx context.Context, tx *sql.Tx, p *SyncFailureParams, now time.Time,
) (string, *int64, int) {
	var currentFailures int
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT failure_count FROM sync_failures WHERE path = ? AND drive_id = ?`,
		p.Path, p.DriveID.String(),
	).Scan(&currentFailures); scanErr != nil && scanErr != sql.ErrNoRows {
		// Non-critical: if we can't read the count, start from 0.
		// The UPSERT will still work correctly.
		m.logger.Warn("RecordFailure: failed to read failure count, using 0",
			slog.String("path", p.Path),
			slog.String("error", scanErr.Error()),
		)
	}

	if p.IssueType != "" && isActionableIssue(p.IssueType) {
		return strActionable, nil, currentFailures
	}

	retryAt := computeNextRetry(now, currentFailures)
	nanos := retryAt.UnixNano()

	return strTransient, &nanos, currentFailures
}

// transitionRemoteStateOnFailure transitions remote_state status for
// download/delete failures (downloading→download_failed, deleting→delete_failed).
// The WHERE clause is a safe no-op when no matching row exists.
func (m *SyncStore) transitionRemoteStateOnFailure(ctx context.Context, tx *sql.Tx, path string) error {
	result, execErr := tx.ExecContext(ctx,
		`UPDATE remote_state SET
			sync_status = CASE sync_status
				WHEN ? THEN ?
				WHEN ? THEN ?
			END
		WHERE path = ? AND sync_status IN (?, ?)`,
		statusDownloading, statusDownloadFailed,
		statusDeleting, statusDeleteFailed,
		path, statusDownloading, statusDeleting,
	)
	if execErr != nil {
		return fmt.Errorf("sync: transitioning remote_state for %s: %w", path, execErr)
	}

	affected, rowErr := result.RowsAffected()
	if rowErr != nil {
		return fmt.Errorf("sync: checking rows affected for %s: %w", path, rowErr)
	}

	if affected == 0 {
		m.logger.Debug("RecordFailure: remote_state row already transitioned or absent",
			slog.String("path", path),
		)
	}

	return nil
}

// computeNextRetry calculates the next retry time with exponential backoff
// and jitter using the unified retry.Reconcile policy.
func computeNextRetry(now time.Time, failureCount int) time.Time {
	return now.Add(retry.Reconcile.Delay(failureCount))
}

// ---------------------------------------------------------------------------
// StateReader methods
// ---------------------------------------------------------------------------

// ListUnreconciled returns remote_state rows that need action (not synced,
// filtered, or deleted).
func (m *SyncStore) ListUnreconciled(ctx context.Context) ([]RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state WHERE sync_status NOT IN (?, ?, ?)`,
		statusSynced, statusFiltered, statusDeleted,
	)
}

// ListActionableRemoteState returns remote_state rows with pending or failed status
// that don't have pending sync_failures (used for initial dispatch, not retry scheduling).
func (m *SyncStore) ListActionableRemoteState(ctx context.Context) ([]RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state
		WHERE sync_status IN (?, ?, ?, ?)`,
		statusPendingDownload, statusDownloadFailed, statusPendingDelete, statusDeleteFailed,
	)
}

// queryRemoteStateRows is a shared helper for scanning multiple remote_state rows.
func (m *SyncStore) queryRemoteStateRows(ctx context.Context, query string, args ...any) ([]RemoteStateRow, error) {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close()

	var result []RemoteStateRow

	for rows.Next() {
		var (
			row      RemoteStateRow
			parentID sql.NullString
			hash     sql.NullString
			size     sql.NullInt64
			mtime    sql.NullInt64
			etag     sql.NullString
			prevPath sql.NullString
		)

		if err := rows.Scan(
			&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
			&hash, &size, &mtime, &etag,
			&prevPath, &row.SyncStatus, &row.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning remote_state row: %w", err)
		}

		row.ParentID = parentID.String
		row.Hash = hash.String
		row.ETag = etag.String
		row.PreviousPath = prevPath.String

		if size.Valid {
			row.Size = size.Int64
		}

		if mtime.Valid {
			row.Mtime = mtime.Int64
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating remote_state rows: %w", err)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// StateAdmin methods
// ---------------------------------------------------------------------------

// ResetFailure resets a single failed path: transitions remote_state back to
// pending and removes the sync_failures row.
func (m *SyncStore) ResetFailure(ctx context.Context, path string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET
			sync_status = ?
		WHERE path = ? AND sync_status IN (?, ?)`,
		statusPendingDownload,
		path, statusDownloadFailed, statusDeleteFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting failure for %s: %w", path, err)
	}

	// Also remove from sync_failures.
	_, err = m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ResetAllFailures resets all failed rows: transitions remote_state back to
// pending and clears all transient sync_failures.
func (m *SyncStore) ResetAllFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDownload, statusDownloadFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting download failures: %w", err)
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDelete, statusDeleteFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting delete failures: %w", err)
	}

	// Clear all transient failures from sync_failures.
	_, err = m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'transient'`)
	if err != nil {
		return fmt.Errorf("sync: clearing transient sync failures: %w", err)
	}

	return nil
}

// ResetInProgressStates is crash recovery: downloading→pending_download.
// For deleting rows, checks the filesystem: file absent → deleted (complete
// the delete), file exists → pending_delete (re-attempt). Called at engine
// startup with the sync root path.
func (m *SyncStore) ResetInProgressStates(ctx context.Context, syncRoot string) error {
	// downloading → pending_download (unconditional, same as before).
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDownload, statusDownloading,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting downloading states: %w", err)
	}

	// deleting → check filesystem to determine correct target state.
	rows, err := m.db.QueryContext(ctx,
		`SELECT drive_id, item_id, path FROM remote_state WHERE sync_status = ?`,
		statusDeleting,
	)
	if err != nil {
		return fmt.Errorf("sync: querying deleting states: %w", err)
	}
	defer rows.Close()

	type deletingRow struct {
		driveID, itemID, path string
	}

	var deletingRows []deletingRow

	for rows.Next() {
		var r deletingRow
		if scanErr := rows.Scan(&r.driveID, &r.itemID, &r.path); scanErr != nil {
			return fmt.Errorf("sync: scanning deleting row: %w", scanErr)
		}

		deletingRows = append(deletingRows, r)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sync: iterating deleting rows: %w", err)
	}

	for _, r := range deletingRows {
		fullPath := filepath.Join(syncRoot, r.path)

		var newStatus string
		if _, statErr := os.Stat(fullPath); statErr != nil {
			// File absent (deleted successfully before crash).
			newStatus = statusDeleted
		} else {
			// File still exists (delete didn't complete).
			newStatus = statusPendingDelete
		}

		if _, execErr := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			newStatus, r.driveID, r.itemID,
		); execErr != nil {
			return fmt.Errorf("sync: resetting deleting state for %s: %w", r.path, execErr)
		}
	}

	return nil
}

// SetDispatchStatus transitions a remote_state row from pending/failed to
// in-progress before the action is dispatched to the worker pool. Uses
// optimistic concurrency: only updates if the current status is valid for
// the given action type.
func (m *SyncStore) SetDispatchStatus(ctx context.Context, driveID, itemID string, actionType ActionType) error {
	switch actionType { //nolint:exhaustive // only download and delete dispatches touch remote_state
	case ActionDownload:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			statusDownloading,
			driveID, itemID, statusPendingDownload, statusDownloadFailed,
		)
		if err != nil {
			return fmt.Errorf("sync: setting dispatch status downloading for %s: %w", itemID, err)
		}

	case ActionLocalDelete:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			statusDeleting,
			driveID, itemID, statusPendingDelete, statusDeleteFailed,
		)
		if err != nil {
			return fmt.Errorf("sync: setting dispatch status deleting for %s: %w", itemID, err)
		}
	}

	return nil
}

// WriteSyncMetadata persists sync metadata after a completed RunOnce pass.
// Keys: last_sync_time, last_sync_duration_ms, last_sync_error,
// last_sync_succeeded, last_sync_failed.
func (m *SyncStore) WriteSyncMetadata(ctx context.Context, report *SyncReport) error {
	now := m.nowFunc().UTC().Format(time.RFC3339)
	durationMS := fmt.Sprintf("%d", report.Duration.Milliseconds())
	succeeded := fmt.Sprintf("%d", report.Succeeded)
	failed := fmt.Sprintf("%d", report.Failed)

	syncErr := ""
	if len(report.Errors) > 0 {
		syncErr = report.Errors[0].Error()
	}

	pairs := [][2]string{
		{"last_sync_time", now},
		{"last_sync_duration_ms", durationMS},
		{"last_sync_error", syncErr},
		{"last_sync_succeeded", succeeded},
		{"last_sync_failed", failed},
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync metadata begin tx: %w", err)
	}
	defer tx.Rollback() // rollback after commit is benign

	const upsertSQL = `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`

	for _, kv := range pairs {
		if _, err := tx.ExecContext(ctx, upsertSQL, kv[0], kv[1]); err != nil {
			return fmt.Errorf("sync metadata upsert %s: %w", kv[0], err)
		}
	}

	return tx.Commit()
}

// ReadSyncMetadata retrieves all sync metadata key-value pairs.
// Returns an empty map if the table doesn't exist or has no rows.
func (m *SyncStore) ReadSyncMetadata(ctx context.Context) (map[string]string, error) {
	result := make(map[string]string)

	rows, err := m.db.QueryContext(ctx, `SELECT key, value FROM sync_metadata`)
	if err != nil {
		// Table might not exist in pre-migration DBs — return empty map.
		return result, nil //nolint:nilerr // graceful fallback for old DBs
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("sync metadata scan: %w", err)
		}

		result[k] = v
	}

	return result, rows.Err()
}

// BaselineEntryCount returns the number of entries in the baseline table.
func (m *SyncStore) BaselineEntryCount(ctx context.Context) (int, error) {
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count); err != nil {
		return 0, fmt.Errorf("baseline entry count: %w", err)
	}

	return count, nil
}

// UnresolvedConflictCount returns the number of unresolved conflicts.
func (m *SyncStore) UnresolvedConflictCount(ctx context.Context) (int, error) {
	var count int

	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("unresolved conflict count: %w", err)
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Nullable helpers: empty string / zero int → NULL in SQLite.
// ---------------------------------------------------------------------------

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}

// nullInt64 maps Go zero (0) to SQL NULL. This conflates "actual zero" with
// "absent" — acceptable for Size (zero-byte files are rare edge cases) and
// Mtime (Unix epoch is not a realistic modification time). If a legitimate
// zero value needs to be stored in the future, use a separate sentinel.
func nullInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: n, Valid: true}
}

// ---------------------------------------------------------------------------
// SyncFailureRecorder methods
// ---------------------------------------------------------------------------

// actionableIssues is the set of issue types that require user action and
// should not be auto-retried. Transient issues (e.g. IssueServiceOutage)
// are NOT in this set — they auto-resolve when the external condition clears.
// Add new actionable issue types here; no other code changes needed.
var actionableIssues = map[string]struct{}{
	IssueInvalidFilename:       {},
	IssuePathTooLong:           {},
	IssueFileTooLarge:          {},
	IssuePermissionDenied:      {},
	IssueQuotaExceeded:         {},
	IssueLocalPermissionDenied: {},
	IssueCaseCollision:         {},
	IssueDiskFull:              {},
	IssueFileTooLargeForSpace:  {},
}

// isActionableIssue returns true for issue types that require user action.
func isActionableIssue(issueType string) bool {
	_, ok := actionableIssues[issueType]
	return ok
}

// ListSyncFailures returns all sync_failures rows ordered by last_seen_at DESC.
func (m *SyncStore) ListSyncFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ListActionableFailures returns sync_failures rows where category is actionable.
// Used by the issues command to show user-actionable file issues.
func (m *SyncStore) ListActionableFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable' ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing actionable sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ClearSyncFailure removes a specific sync_failures row by path and drive.
func (m *SyncStore) ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ? AND drive_id = ?`,
		path, driveID.String())
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ClearSyncFailureByPath removes all sync_failures rows for a path regardless
// of drive. Used by CLI commands where the drive context isn't known.
func (m *SyncStore) ClearSyncFailureByPath(ctx context.Context, path string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ClearActionableSyncFailures removes all actionable sync_failures rows.
func (m *SyncStore) ClearActionableSyncFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'actionable'`)
	if err != nil {
		return fmt.Errorf("sync: clearing resolved sync failures: %w", err)
	}

	return nil
}

// MarkSyncFailureActionable sets a sync_failures row to category='actionable'
// and clears its next_retry_at.
func (m *SyncStore) MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE sync_failures SET category = 'actionable', next_retry_at = NULL
		WHERE path = ? AND drive_id = ?`,
		path, driveID.String())
	if err != nil {
		return fmt.Errorf("sync: marking sync failure actionable for %s: %w", path, err)
	}

	return nil
}

// UpsertActionableFailures batch-upserts scanner-detected issues into
// sync_failures as actionable entries. Each failure is inserted with
// failure_count=1 and no next_retry_at. On conflict, the existing row is
// updated with the latest error info.
func (m *SyncStore) UpsertActionableFailures(ctx context.Context, failures []ActionableFailure) error {
	if len(failures) == 0 {
		return nil
	}

	now := m.nowFunc()
	nowNano := now.UnixNano()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin upsert actionable failures: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// NOTE: RecordFailure has a parallel INSERT with different ON CONFLICT
	// semantics (COALESCE, failure_count increment) — update both when
	// adding columns.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO sync_failures
			(path, drive_id, direction, category, issue_type, item_id,
			 failure_count, next_retry_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, 'actionable', ?, '', 1, NULL, ?, 0, ?, ?, ?, '', ?)
		ON CONFLICT(path, drive_id) DO UPDATE SET
			direction = excluded.direction,
			category = 'actionable',
			issue_type = excluded.issue_type,
			next_retry_at = NULL,
			last_error = excluded.last_error,
			last_seen_at = excluded.last_seen_at,
			file_size = excluded.file_size,
			scope_key = excluded.scope_key`)
	if err != nil {
		return fmt.Errorf("sync: prepare upsert actionable: %w", err)
	}
	defer stmt.Close()

	for i := range failures {
		f := &failures[i]
		if _, err := stmt.ExecContext(ctx,
			f.Path, f.DriveID.String(), f.Direction,
			nullString(f.IssueType), f.Error,
			nowNano, nowNano, f.FileSize, f.ScopeKey,
		); err != nil {
			return fmt.Errorf("sync: upsert actionable failure for %s: %w", f.Path, err)
		}
	}

	return tx.Commit()
}

// ClearResolvedActionableFailures removes actionable sync_failures entries of
// the given issue type whose paths are NOT in currentPaths. This cleans up
// issues that the scanner no longer reports (the underlying cause was resolved).
func (m *SyncStore) ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error {
	if issueType == "" {
		return nil
	}

	// Build a set of currently-skipped paths for IN clause.
	if len(currentPaths) == 0 {
		// No current paths → all entries for this issue type are resolved.
		_, err := m.db.ExecContext(ctx,
			`DELETE FROM sync_failures WHERE category = 'actionable' AND issue_type = ?`,
			issueType)
		if err != nil {
			return fmt.Errorf("sync: clearing resolved actionable failures for %s: %w", issueType, err)
		}

		return nil
	}

	// Build parameterized IN clause with literal placeholders.
	placeholders := "?" + strings.Repeat(",?", len(currentPaths)-1)
	args := make([]any, 0, len(currentPaths)+1)
	args = append(args, issueType)

	for _, p := range currentPaths {
		args = append(args, p)
	}

	//nolint:gosec // G202: placeholders is strings.Repeat(",?", n) — literal, not user input
	query := `DELETE FROM sync_failures WHERE category = 'actionable' AND issue_type = ? AND path NOT IN (` +
		placeholders + `)`

	_, err := m.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sync: clearing resolved actionable failures for %s: %w", issueType, err)
	}

	return nil
}

// ListSyncFailuresForRetry returns transient sync_failures rows whose
// next_retry_at has expired (ready for retry).
func (m *SyncStore) ListSyncFailuresForRetry(ctx context.Context, now time.Time) ([]SyncFailureRow, error) {
	nowNano := now.UnixNano()
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at <= ?`,
		nowNano)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures for retry: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// EarliestSyncFailureRetryAt returns the minimum future next_retry_at across
// transient sync_failures. Returns zero time if none exist.
func (m *SyncStore) EarliestSyncFailureRetryAt(ctx context.Context, now time.Time) (time.Time, error) {
	nowNano := now.UnixNano()

	var minNano *int64
	err := m.db.QueryRowContext(ctx,
		`SELECT MIN(next_retry_at) FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at > ?`,
		nowNano,
	).Scan(&minNano)
	if err != nil {
		return time.Time{}, fmt.Errorf("sync: querying earliest sync failure retry: %w", err)
	}

	if minNano == nil {
		return time.Time{}, nil
	}

	return time.Unix(0, *minNano), nil
}

// SyncFailureCount returns the number of transient sync_failures rows.
func (m *SyncStore) SyncFailureCount(ctx context.Context) (int, error) {
	var count int

	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_failures WHERE category = 'transient'`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sync: counting sync failures: %w", err)
	}

	return count, nil
}

// ListSyncFailuresByIssueType returns all sync_failures rows with the given issue_type.
func (m *SyncStore) ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE issue_type = ? ORDER BY last_seen_at DESC`, issueType)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures by type %s: %w", issueType, err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ClearSyncFailuresByPrefix removes all sync_failures rows whose path starts
// with the given prefix and matches the given issue_type.
func (m *SyncStore) ClearSyncFailuresByPrefix(ctx context.Context, pathPrefix, issueType string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE issue_type = ? AND (path = ? OR path LIKE ?)`,
		issueType, pathPrefix, pathPrefix+"/%")
	if err != nil {
		return fmt.Errorf("sync: clearing sync failures by prefix %s: %w", pathPrefix, err)
	}

	return nil
}

// scanSyncFailureRows scans multiple sync_failures rows from a query result.
func scanSyncFailureRows(rows *sql.Rows) ([]SyncFailureRow, error) {
	var result []SyncFailureRow

	for rows.Next() {
		var r SyncFailureRow
		if scanErr := rows.Scan(
			&r.Path, &r.DriveID, &r.Direction, &r.Category,
			&r.IssueType, &r.ItemID,
			&r.FailureCount, &r.NextRetryAt,
			&r.LastError, &r.HTTPStatus,
			&r.FirstSeenAt, &r.LastSeenAt,
			&r.FileSize, &r.LocalHash,
			&r.ScopeKey,
		); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning sync failure row: %w", scanErr)
		}

		result = append(result, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating sync failure rows: %w", err)
	}

	return result, nil
}
