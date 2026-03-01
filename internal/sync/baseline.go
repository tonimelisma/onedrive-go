package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	// Pure-Go SQLite driver (no CGO).
	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT path, drive_id, item_id, parent_id, item_type,
		local_hash, remote_hash, size, mtime, synced_at, etag
		FROM baseline`

	sqlGetDeltaToken = `SELECT token FROM delta_tokens WHERE drive_id = ?` //nolint:gosec // G101: "token" is a delta cursor, not credentials

	sqlUpsertBaseline = `INSERT INTO baseline
		(path, drive_id, item_id, parent_id, item_type, local_hash, remote_hash,
		 size, mtime, synced_at, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		 drive_id = excluded.drive_id,
		 item_id = excluded.item_id,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 size = excluded.size,
		 mtime = excluded.mtime,
		 synced_at = excluded.synced_at,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`

	sqlInsertConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime,
		 resolution, resolved_at, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpsertDeltaToken = `INSERT INTO delta_tokens (drive_id, token, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(drive_id) DO UPDATE SET
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

// BaselineManager is the sole writer to the sync database. It loads the
// baseline at cycle start and commits outcomes at cycle end.
type BaselineManager struct {
	db       *sql.DB
	baseline *Baseline
	logger   *slog.Logger
	nowFunc  func() time.Time // injectable for deterministic tests
}

// NewBaselineManager opens the SQLite database at dbPath, runs migrations,
// and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewBaselineManager(dbPath string, logger *slog.Logger) (*BaselineManager, error) {
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

	return &BaselineManager{
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
// because BaselineManager exclusively owns the database (sole-writer
// pattern with SetMaxOpenConns(1)).
func (m *BaselineManager) Load(ctx context.Context) (*Baseline, error) {
	if m.baseline != nil {
		return m.baseline, nil
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}
	defer rows.Close()

	b := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   make(map[driveid.ItemKey]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows)
		if err != nil {
			return nil, err
		}

		b.ByPath[entry.Path] = entry
		b.ByID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating baseline rows: %w", err)
	}

	m.baseline = b
	m.logger.Debug("baseline loaded", slog.Int("entries", len(b.ByPath)))

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
		&e.Path, &e.DriveID, &e.ItemID, &parentID, &itemType,
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

// GetDeltaToken returns the saved delta token for a drive, or empty string
// if no token has been saved yet.
func (m *BaselineManager) GetDeltaToken(ctx context.Context, driveID string) (string, error) {
	var token string

	err := m.db.QueryRowContext(ctx, sqlGetDeltaToken, driveID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: getting delta token for drive %s: %w", driveID, err)
	}

	return token, nil
}

// CommitOutcome atomically applies a single outcome to the baseline in a
// SQLite transaction. After the DB write, the in-memory baseline cache is
// updated incrementally (Put or Delete).
func (m *BaselineManager) CommitOutcome(ctx context.Context, outcome *Outcome) error {
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
	switch o.Action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		return commitUpsert(ctx, tx, o, syncedAt)
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		return commitDelete(ctx, tx, o.Path)
	case ActionLocalMove, ActionRemoteMove:
		return commitMove(ctx, tx, o, syncedAt)
	case ActionConflict:
		return commitConflict(ctx, tx, o, syncedAt)
	default:
		return nil
	}
}

// updateBaselineCache applies a single outcome to the in-memory baseline,
// keeping the cache consistent without a full DB reload.
func (m *BaselineManager) updateBaselineCache(o *Outcome, syncedAt int64) {
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
// from baseline updates. Used after all actions in a cycle complete.
func (m *BaselineManager) CommitDeltaToken(ctx context.Context, token, driveID string) error {
	if token == "" {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning delta token transaction: %w", err)
	}
	defer tx.Rollback()

	updatedAt := m.nowFunc().UnixNano()
	if saveErr := m.saveDeltaToken(ctx, tx, driveID, token, updatedAt); saveErr != nil {
		return saveErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("sync: committing delta token transaction: %w", commitErr)
	}

	m.logger.Debug("delta token committed",
		slog.String("drive_id", driveID),
	)

	return nil
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes.
func commitUpsert(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	_, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		o.Path, o.DriveID, o.ItemID,
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

// commitMove deletes the old path and inserts the new path for move outcomes.
func commitMove(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	if err := commitDelete(ctx, tx, o.OldPath); err != nil {
		return err
	}

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

// saveDeltaToken persists the delta token in the same transaction as
// baseline updates.
func (m *BaselineManager) saveDeltaToken(
	ctx context.Context, tx *sql.Tx, driveID, token string, updatedAt int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpsertDeltaToken, driveID, token, updatedAt)
	if err != nil {
		return fmt.Errorf("sync: saving delta token for drive %s: %w", driveID, err)
	}

	return nil
}

// ListConflicts returns all unresolved conflicts ordered by detection time.
func (m *BaselineManager) ListConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListConflicts)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) ordered
// by detection time descending. Used by 'conflicts --history'.
func (m *BaselineManager) ListAllConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListAllConflicts)
}

// queryConflicts executes a conflict query and scans the results.
func (m *BaselineManager) queryConflicts(ctx context.Context, query string) ([]ConflictRecord, error) {
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
func (m *BaselineManager) GetConflict(ctx context.Context, idOrPath string) (*ConflictRecord, error) {
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
func (m *BaselineManager) ResolveConflict(ctx context.Context, id, resolution string) error {
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

// DB returns the underlying database connection for sharing with other
// components that need to participate in the same database.
func (m *BaselineManager) DB() *sql.DB {
	return m.db
}

// Close closes the underlying database connection.
func (m *BaselineManager) Close() error {
	return m.db.Close()
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
