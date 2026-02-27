package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Package-level API documentation for ledger.go (B-145):
//
// Ledger provides crash-recoverable persistence for sync actions via the
// action_queue SQLite table. It shares a *sql.DB with BaselineManager
// (sole-writer via SetMaxOpenConns(1)). The lifecycle is:
//
//	WriteActions → Claim → Complete/Fail/Cancel
//
// WriteActions inserts a batch of actions atomically. Workers call Claim
// before execution, then Complete or Fail afterward. CancelByPath in the
// DepTracker calls Cancel on the ledger row. LoadPending + ReclaimStale
// handle crash recovery (Phase 5.3).
//
// All status transitions are enforced: Claim requires "pending", Complete
// and Fail require "claimed". Cancel can transition from any status.

// Ledger status constants for action_queue.status column.
const (
	ledgerStatusPending  = "pending"
	ledgerStatusClaimed  = "claimed"
	ledgerStatusDone     = "done"
	ledgerStatusFailed   = "failed"
	ledgerStatusCanceled = "canceled"
)

// LedgerRow represents a single action from the action_queue table,
// returned by LoadPending for crash recovery.
type LedgerRow struct {
	ID         int64
	CycleID    string
	ActionType string
	Path       string
	OldPath    string
	Status     string
	DependsOn  []int64 // parsed from JSON array in depends_on column
	DriveID    string
	ItemID     string
	ParentID   string
	Hash       string
	Size       int64
	Mtime      int64
	SessionURL string // upload session URL for resume (B-037)
	BytesDone  int64
	ErrorMsg   string
}

// Ledger manages the action_queue table, providing crash-recoverable
// persistence for in-flight actions. It shares the *sql.DB with
// BaselineManager (sole-writer pattern via SetMaxOpenConns(1)).
type Ledger struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewLedger creates a Ledger that shares the given database connection.
func NewLedger(db *sql.DB, logger *slog.Logger) *Ledger {
	return &Ledger{db: db, logger: logger}
}

// WriteActions inserts all actions as pending rows in a single transaction,
// encoding dependency indices as a JSON array in depends_on. Returns the
// database IDs of the inserted rows (in the same order as actions).
//
// NOTE: The depends_on column stores planner-assigned indices (0-based
// positions in the actions slice), NOT ledger IDs. The in-memory DepTracker
// maps indices → ledger IDs before building the dependency graph. For crash
// recovery (Phase 5.3), the mapping is deterministic: all actions in a cycle
// are inserted in a single transaction with sequential IDs, so
// ledgerID = firstID + index.
func (l *Ledger) WriteActions(
	ctx context.Context, actions []Action, deps [][]int, cycleID string,
) ([]int64, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sync: ledger begin write: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO action_queue
			(cycle_id, action_type, path, old_path, status, depends_on,
			 drive_id, item_id, parent_id, hash, size, mtime)
			VALUES (?, ?, ?, ?, '`+ledgerStatusPending+`', ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("sync: ledger prepare: %w", err)
	}
	defer stmt.Close()

	ids := make([]int64, len(actions))

	for i := range actions {
		a := &actions[i]

		var depsJSON sql.NullString
		if len(deps) > i && len(deps[i]) > 0 {
			b, jsonErr := json.Marshal(deps[i])
			if jsonErr != nil {
				return nil, fmt.Errorf("sync: encoding deps for action %d: %w", i, jsonErr)
			}

			depsJSON = sql.NullString{String: string(b), Valid: true}
		}

		// Action.Path is the canonical path (destination for moves),
		// Action.OldPath is the source (moves only). No swap needed —
		// matches the ledger schema directly (path=destination, old_path=source).
		pathVal := a.Path
		oldPathVal := a.OldPath

		result, execErr := stmt.ExecContext(ctx, cycleID,
			a.Type.String(), pathVal, nullString(oldPathVal), depsJSON,
			nullString(a.DriveID.String()), nullString(a.ItemID),
			resolveParentIDFromView(a),
			resolveHashFromView(a),
			resolveSize(a),
			resolveMtime(a),
		)
		if execErr != nil {
			return nil, fmt.Errorf("sync: ledger insert action %d (%s): %w", i, a.Path, execErr)
		}

		id, idErr := result.LastInsertId()
		if idErr != nil {
			return nil, fmt.Errorf("sync: ledger last insert ID: %w", idErr)
		}

		ids[i] = id
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: ledger commit write: %w", err)
	}

	l.logger.Info("ledger: actions written",
		slog.Int("count", len(actions)),
		slog.String("cycle_id", cycleID),
	)

	return ids, nil
}

// Claim transitions an action from pending to claimed.
func (l *Ledger) Claim(ctx context.Context, id int64) error {
	now := time.Now().UnixNano()

	result, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET status = '`+ledgerStatusClaimed+`', claimed_at = ?
		 WHERE id = ? AND status = '`+ledgerStatusPending+`'`, now, id)
	if err != nil {
		return fmt.Errorf("sync: ledger claim %d: %w", id, err)
	}

	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("sync: ledger claim %d rows affected: %w", id, rowsErr)
	}

	if rows == 0 {
		return fmt.Errorf("sync: ledger claim %d: action not %s", id, ledgerStatusPending)
	}

	return nil
}

// Complete transitions an action from claimed to done.
func (l *Ledger) Complete(ctx context.Context, id int64) error {
	now := time.Now().UnixNano()

	result, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET status = '`+ledgerStatusDone+`', completed_at = ?
		 WHERE id = ? AND status = '`+ledgerStatusClaimed+`'`, now, id)
	if err != nil {
		return fmt.Errorf("sync: ledger complete %d: %w", id, err)
	}

	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("sync: ledger complete %d rows affected: %w", id, rowsErr)
	}

	if rows == 0 {
		return fmt.Errorf("sync: ledger complete %d: action not %s", id, ledgerStatusClaimed)
	}

	return nil
}

// Fail transitions an action from claimed to failed, recording the error.
func (l *Ledger) Fail(ctx context.Context, id int64, errMsg string) error {
	now := time.Now().UnixNano()

	result, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET status = '`+ledgerStatusFailed+`', completed_at = ?, error_msg = ?
		 WHERE id = ? AND status = '`+ledgerStatusClaimed+`'`, now, errMsg, id)
	if err != nil {
		return fmt.Errorf("sync: ledger fail %d: %w", id, err)
	}

	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("sync: ledger fail %d rows affected: %w", id, rowsErr)
	}

	if rows == 0 {
		return fmt.Errorf("sync: ledger fail %d: action not %s", id, ledgerStatusClaimed)
	}

	return nil
}

// Cancel transitions an action to canceled from any status.
func (l *Ledger) Cancel(ctx context.Context, id int64) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET status = '`+ledgerStatusCanceled+`' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sync: ledger cancel %d: %w", id, err)
	}

	return nil
}

// LoadPending returns all non-terminal (pending or claimed) rows for a cycle.
func (l *Ledger) LoadPending(ctx context.Context, cycleID string) ([]LedgerRow, error) {
	return l.queryRows(ctx,
		`WHERE cycle_id = ? AND status IN ('`+ledgerStatusPending+`', '`+ledgerStatusClaimed+`')`,
		"load pending", cycleID)
}

// LoadAllPending returns all non-terminal (pending or claimed) rows across
// all cycles, ordered by id. Used for crash recovery at engine startup.
func (l *Ledger) LoadAllPending(ctx context.Context) ([]LedgerRow, error) {
	return l.queryRows(ctx,
		`WHERE status IN ('`+ledgerStatusPending+`', '`+ledgerStatusClaimed+`')`,
		"load all pending")
}

// ReclaimStale resets claimed actions older than timeout back to pending.
// Returns the number of reclaimed actions.
func (l *Ledger) ReclaimStale(ctx context.Context, timeout time.Duration) (int, error) {
	cutoff := time.Now().Add(-timeout).UnixNano()

	result, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET status = '`+ledgerStatusPending+`', claimed_at = NULL
		 WHERE status = '`+ledgerStatusClaimed+`' AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sync: ledger reclaim stale: %w", err)
	}

	n, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return 0, fmt.Errorf("sync: ledger reclaim rows affected: %w", rowsErr)
	}

	if n > 0 {
		l.logger.Warn("ledger: reclaimed stale actions",
			slog.Int64("count", n),
			slog.Duration("timeout", timeout),
		)
	}

	return int(n), nil
}

// CountPendingForCycle returns the count of non-terminal actions for a cycle.
func (l *Ledger) CountPendingForCycle(ctx context.Context, cycleID string) (int, error) {
	var count int

	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM action_queue
		 WHERE cycle_id = ? AND status IN ('`+ledgerStatusPending+`', '`+ledgerStatusClaimed+`')`, cycleID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sync: ledger count pending: %w", err)
	}

	return count, nil
}

// CountFailedForCycle returns the count of failed actions for a cycle.
// Used by watchCycleCompletion to decide whether to commit the delta token.
func (l *Ledger) CountFailedForCycle(ctx context.Context, cycleID string) (int, error) {
	var count int

	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM action_queue
		 WHERE cycle_id = ? AND status = '`+ledgerStatusFailed+`'`, cycleID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sync: ledger count failed: %w", err)
	}

	return count, nil
}

// UpdateSessionURL stores the upload session URL for a claimed action.
// Used for upload session resume after crash recovery (B-037).
func (l *Ledger) UpdateSessionURL(ctx context.Context, id int64, sessionURL string) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET session_url = ? WHERE id = ?`, sessionURL, id)
	if err != nil {
		return fmt.Errorf("sync: ledger update session URL %d: %w", id, err)
	}

	return nil
}

// UpdateBytesDone records the cumulative bytes uploaded for a claimed action.
// Called periodically during chunked uploads for progress tracking.
func (l *Ledger) UpdateBytesDone(ctx context.Context, id int64, bytesDone int64) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE action_queue SET bytes_done = ? WHERE id = ?`, bytesDone, id)
	if err != nil {
		return fmt.Errorf("sync: ledger update bytes done %d: %w", id, err)
	}

	return nil
}

// LoadCycleResults returns all terminal (done or failed) rows for a cycle.
// Used by the engine to record successes/failures for B-123 suppression.
func (l *Ledger) LoadCycleResults(ctx context.Context, cycleID string) ([]LedgerRow, error) {
	return l.queryRows(ctx,
		`WHERE cycle_id = ? AND status IN ('`+ledgerStatusDone+`', '`+ledgerStatusFailed+`')`,
		"load cycle results", cycleID)
}

// ledgerSelectCols is the column list shared by all ledger row queries.
const ledgerSelectCols = `SELECT id, cycle_id, action_type, path, old_path, status,
	depends_on, drive_id, item_id, parent_id, hash, size, mtime,
	session_url, bytes_done, error_msg
 FROM action_queue `

// queryRows executes a parameterized query against the action_queue table
// and returns the scanned rows. The whereClause is appended after the base
// SELECT. The desc is used in error messages for debugging.
func (l *Ledger) queryRows(ctx context.Context, whereClause, desc string, args ...any) ([]LedgerRow, error) {
	query := ledgerSelectCols + whereClause + ` ORDER BY id` //nolint:gosec // whereClause is always a compile-time constant, never user input

	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: ledger %s: %w", desc, err)
	}
	defer rows.Close()

	var result []LedgerRow

	for rows.Next() {
		r, scanErr := scanLedgerRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		result = append(result, *r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: ledger iterating %s rows: %w", desc, err)
	}

	return result, nil
}

// scanLedgerRow scans a single row into a LedgerRow, parsing depends_on JSON.
func scanLedgerRow(rows *sql.Rows) (*LedgerRow, error) {
	var (
		r          LedgerRow
		oldPath    sql.NullString
		depsJSON   sql.NullString
		driveID    sql.NullString
		itemID     sql.NullString
		parentID   sql.NullString
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		sessionURL sql.NullString
		bytesDone  sql.NullInt64
		errorMsg   sql.NullString
	)

	err := rows.Scan(
		&r.ID, &r.CycleID, &r.ActionType, &r.Path, &oldPath, &r.Status,
		&depsJSON, &driveID, &itemID, &parentID, &hash, &size, &mtime,
		&sessionURL, &bytesDone, &errorMsg,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning ledger row: %w", err)
	}

	r.OldPath = oldPath.String
	r.DriveID = driveID.String
	r.ItemID = itemID.String
	r.ParentID = parentID.String
	r.Hash = hash.String
	r.SessionURL = sessionURL.String
	r.ErrorMsg = errorMsg.String

	if size.Valid {
		r.Size = size.Int64
	}

	if mtime.Valid {
		r.Mtime = mtime.Int64
	}

	if bytesDone.Valid {
		r.BytesDone = bytesDone.Int64
	}

	if depsJSON.Valid && depsJSON.String != "" {
		if jsonErr := json.Unmarshal([]byte(depsJSON.String), &r.DependsOn); jsonErr != nil {
			return nil, fmt.Errorf("sync: parsing depends_on for action %d: %w", r.ID, jsonErr)
		}
	}

	return &r, nil
}

// resolveParentIDFromView extracts parent ID from the action's view.
func resolveParentIDFromView(a *Action) string {
	if a.View != nil && a.View.Remote != nil {
		return a.View.Remote.ParentID
	}

	if a.View != nil && a.View.Baseline != nil {
		return a.View.Baseline.ParentID
	}

	return ""
}

// resolveHashFromView extracts a hash from the action's view. For uploads,
// the local hash is preferred (Remote may be nil for new files). For all
// other action types, remote hash is preferred.
func resolveHashFromView(a *Action) string {
	if a.Type == ActionUpload {
		if a.View != nil && a.View.Local != nil && a.View.Local.Hash != "" {
			return a.View.Local.Hash
		}
	}

	if a.View != nil && a.View.Remote != nil {
		return a.View.Remote.Hash
	}

	return ""
}

// resolveSize extracts size from the action's view (remote preferred).
func resolveSize(a *Action) int64 {
	if a.View != nil && a.View.Remote != nil {
		return a.View.Remote.Size
	}

	if a.View != nil && a.View.Local != nil {
		return a.View.Local.Size
	}

	return 0
}

// resolveMtime extracts mtime from the action's view (remote preferred).
func resolveMtime(a *Action) int64 {
	if a.View != nil && a.View.Remote != nil {
		return a.View.Remote.Mtime
	}

	if a.View != nil && a.View.Local != nil {
		return a.View.Local.Mtime
	}

	return 0
}

// ParseActionType converts a database TEXT value to ActionType.
func ParseActionType(s string) (ActionType, error) {
	switch s {
	case ActionDownload.String():
		return ActionDownload, nil
	case ActionUpload.String():
		return ActionUpload, nil
	case ActionLocalDelete.String():
		return ActionLocalDelete, nil
	case ActionRemoteDelete.String():
		return ActionRemoteDelete, nil
	case ActionLocalMove.String():
		return ActionLocalMove, nil
	case ActionRemoteMove.String():
		return ActionRemoteMove, nil
	case ActionFolderCreate.String():
		return ActionFolderCreate, nil
	case ActionConflict.String():
		return ActionConflict, nil
	case ActionUpdateSynced.String():
		return ActionUpdateSynced, nil
	case ActionCleanup.String():
		return ActionCleanup, nil
	default:
		return ActionDownload, fmt.Errorf("sync: unknown action type %q", s)
	}
}

// LastCycleID returns the most recent cycle_id from the action_queue, or ""
// if the table is empty. Used for crash recovery.
func (l *Ledger) LastCycleID(ctx context.Context) (string, error) {
	var cycleID sql.NullString

	err := l.db.QueryRowContext(ctx,
		`SELECT cycle_id FROM action_queue ORDER BY id DESC LIMIT 1`).Scan(&cycleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: ledger last cycle: %w", err)
	}

	return cycleID.String, nil
}
