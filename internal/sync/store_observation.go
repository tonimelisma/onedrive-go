// store_observation.go — Remote state observation persistence for SyncStore.
//
// Contents:
//   - CommitObservation:        atomically persist remote state + advance delta token
//   - processObservedItem:      handle single observed item within transaction
//   - scanRemoteStateRow:       read one remote_state row
//   - insertRemoteState:        insert new remote_state row
//   - updateRemoteStateFromObs: update existing remote_state from observation
//
// Related files:
//   - store.go:            SyncStore type definition and lifecycle
//   - store_baseline.go:   saveDeltaToken (called from CommitObservation)
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

	// sqlGetRemoteStateByPath uses the idx_remote_state_active_path unique
	// partial index (excludes deleted/pending_delete rows). Retrier and
	// isFailureResolved use this to look up the current remote state for a
	// path without knowing the item_id.
	sqlGetRemoteStateByPath = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, sync_status, observed_at
		FROM remote_state
		WHERE path = ? AND drive_id = ?
		AND sync_status NOT IN ('deleted', 'pending_delete')`
)

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

// GetRemoteStateByPath looks up the active remote_state row for a path+drive
// combination. Uses the idx_remote_state_active_path partial index, which
// excludes deleted/pending_delete rows. Returns (nil, nil) when no active
// row exists — the caller must check for nil.
//
// Used by:
//   - isFailureResolved: to detect whether a download failure's underlying
//     remote state has been resolved (synced/deleted/filtered).
//   - createEventFromDB: to build full-fidelity ChangeEvents for the retrier.
func (m *SyncStore) GetRemoteStateByPath(ctx context.Context, path string, driveID driveid.ID) (*RemoteStateRow, error) {
	var (
		row      RemoteStateRow
		parentID sql.NullString
		hash     sql.NullString
		size     sql.NullInt64
		mtime    sql.NullInt64
		etag     sql.NullString
		prevPath sql.NullString
	)

	err := m.db.QueryRowContext(ctx, sqlGetRemoteStateByPath, path, driveID.String()).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}

		return nil, fmt.Errorf("sync: GetRemoteStateByPath %s: %w", path, err)
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

	return &row, nil
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
