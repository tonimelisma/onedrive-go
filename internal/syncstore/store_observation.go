// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - CommitObservation:        atomically persist remote state + advance delta token
//   - processObservedItem:      handle single observed item within transaction
//   - computeNewStatus:         sync_status state machine (30-cell decision matrix)
//   - scanRemoteStateRow:       read one remote_state row
//   - insertRemoteState:        insert new remote_state row
//   - updateRemoteStateFromObs: update existing remote_state from observation
//
// Related files:
//   - store.go:            SyncStore type definition and lifecycle
//   - store_baseline.go:   saveDeltaToken (called from CommitObservation)
package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// SQL statements for remote_state operations.
const (
	sqlGetRemoteStateRow = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, sync_status, observed_at,
		filter_generation, filter_reason
		FROM remote_state WHERE drive_id = ? AND item_id = ?`

	sqlInsertRemoteState = `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
		 sync_status, observed_at, filter_generation, filter_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpdateRemoteState = `UPDATE remote_state SET
		path = ?, parent_id = ?, item_type = ?, hash = ?, size = ?, mtime = ?, etag = ?,
		previous_path = ?, sync_status = ?, observed_at = ?, filter_generation = ?, filter_reason = ?
		WHERE drive_id = ? AND item_id = ?`

	// sqlGetRemoteStateByPath uses the idx_remote_state_active_path unique
	// partial index (excludes deleted/pending_delete rows). Retrier and
	// isFailureResolved use this to look up the current remote state for a
	// path without knowing the item_id.
	sqlGetRemoteStateByPath = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, sync_status, observed_at,
		filter_generation, filter_reason
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
func (m *SyncStore) CommitObservation(ctx context.Context, events []synctypes.ObservedItem, newToken string, driveID driveid.ID) error {
	return m.commitObservation(ctx, events, newToken, driveID, "")
}

// CommitObservationForScope is the folder-scoped variant of CommitObservation.
// Shared-folder configured drives use scopeID=rootItemID so the selected folder
// owns its own delta token instead of competing with the owner drive root.
func (m *SyncStore) CommitObservationForScope(
	ctx context.Context,
	events []synctypes.ObservedItem,
	newToken string,
	driveID driveid.ID,
	scopeID string,
) error {
	return m.commitObservation(ctx, events, newToken, driveID, scopeID)
}

func (m *SyncStore) commitObservation(
	ctx context.Context,
	events []synctypes.ObservedItem,
	newToken string,
	driveID driveid.ID,
	scopeID string,
) error {
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
		if err := m.saveDeltaToken(ctx, tx, driveID.String(), scopeID, driveID.String(), newToken, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}

	m.logger.Debug("observations committed",
		slog.Int("items", len(events)),
		slog.String("drive_id", driveID.String()),
		slog.String("scope_id", scopeID),
	)

	return nil
}

// processObservedItem handles a single item within the CommitObservation transaction.
func (m *SyncStore) processObservedItem(ctx context.Context, tx *sql.Tx, item *synctypes.ObservedItem, now int64) error {
	existing := m.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		// No existing row — skip deleted items we've never seen.
		if item.IsDeleted {
			return nil
		}

		return m.insertRemoteState(ctx, tx, item, now)
	}

	// Existing row — compute new status via the state machine.
	newStatus, changed := computeNewStatus(existing.SyncStatus, existing.Hash, item.Hash, item.IsDeleted, item.Filtered)

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
func (m *SyncStore) scanRemoteStateRow(ctx context.Context, tx *sql.Tx, driveID, itemID string) *synctypes.RemoteStateRow {
	var (
		row          synctypes.RemoteStateRow
		parentID     sql.NullString
		hash         sql.NullString
		size         sql.NullInt64
		mtime        sql.NullInt64
		etag         sql.NullString
		prevPath     sql.NullString
		filterReason sql.NullString
	)

	err := tx.QueryRowContext(ctx, sqlGetRemoteStateRow, driveID, itemID).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt, &row.FilterGeneration, &filterReason,
	)
	if err != nil {
		return nil
	}

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String
	row.FilterReason = synctypes.RemoteFilterReason(filterReason.String)

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
// excludes deleted/pending_delete rows.
//
// Used by:
//   - isFailureResolved: to detect whether a download failure's underlying
//     remote state has been resolved (synced/deleted/filtered).
//   - createEventFromDB: to build full-fidelity ChangeEvents for the retrier.
func (m *SyncStore) GetRemoteStateByPath(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (*synctypes.RemoteStateRow, bool, error) {
	var (
		row          synctypes.RemoteStateRow
		parentID     sql.NullString
		hash         sql.NullString
		size         sql.NullInt64
		mtime        sql.NullInt64
		etag         sql.NullString
		prevPath     sql.NullString
		filterReason sql.NullString
	)

	err := m.db.QueryRowContext(ctx, sqlGetRemoteStateByPath, path, driveID.String()).Scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &row.SyncStatus, &row.ObservedAt, &row.FilterGeneration, &filterReason,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: GetRemoteStateByPath %s: %w", path, err)
	}

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String
	row.FilterReason = synctypes.RemoteFilterReason(filterReason.String)

	if size.Valid {
		row.Size = size.Int64
	}

	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row, true, nil
}

// insertRemoteState inserts a new remote_state row for a newly observed item.
func (m *SyncStore) insertRemoteState(ctx context.Context, tx *sql.Tx, item *synctypes.ObservedItem, now int64) error {
	initialStatus := synctypes.SyncStatusPendingDownload
	if item.Filtered {
		initialStatus = synctypes.SyncStatusFiltered
	}

	_, err := tx.ExecContext(ctx, sqlInsertRemoteState,
		item.DriveID.String(), item.ItemID, item.Path,
		nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		initialStatus, now, initialFilterGeneration(item), string(item.FilterReason),
	)
	if err != nil {
		return fmt.Errorf("sync: inserting remote_state for %s: %w", item.Path, err)
	}

	return nil
}

// updateRemoteStateFromObs updates an existing remote_state row with observation data.
func (m *SyncStore) updateRemoteStateFromObs(
	ctx context.Context, tx *sql.Tx, item *synctypes.ObservedItem,
	newStatus synctypes.SyncStatus, previousPath string, now int64,
) error {
	filterGeneration := int64(0)
	filterReason := ""
	if newStatus == synctypes.SyncStatusFiltered {
		filterGeneration = initialFilterGeneration(item)
		filterReason = string(item.FilterReason)
	}

	_, err := tx.ExecContext(ctx, sqlUpdateRemoteState,
		item.Path, nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		nullString(previousPath), newStatus, now, filterGeneration, filterReason,
		item.DriveID.String(), item.ItemID,
	)
	if err != nil {
		return fmt.Errorf("sync: updating remote_state for %s: %w", item.Path, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Status state machine — determines sync_status transitions for remote_state
// rows when delta observations arrive.
// ---------------------------------------------------------------------------

// computeNewStatus determines the new sync_status for a remote_state row
// when a delta observation arrives. Pure function — no I/O, no side effects.
//
// Returns (newStatus, changed). changed=false means the row should not be
// updated (the observation is a no-op for this row).
//
// Implements the 30-cell decision matrix from
// spec/design/data-model.md (remote_state sync_status state machine).
func computeNewStatus(
	currentStatus synctypes.SyncStatus,
	currentHash,
	observedHash string,
	isDeleted,
	filtered bool,
) (synctypes.SyncStatus, bool) {
	if filtered && !isDeleted {
		return computeFilteredObservation(currentStatus, currentHash, observedHash)
	}

	sameHash := currentHash == observedHash

	if isDeleted {
		return computeDeleted(currentStatus)
	}

	if sameHash {
		return computeSameHash(currentStatus)
	}

	return computeDifferentHash(currentStatus)
}

func computeFilteredObservation(
	currentStatus synctypes.SyncStatus,
	currentHash,
	observedHash string,
) (synctypes.SyncStatus, bool) {
	if currentStatus == synctypes.SyncStatusFiltered {
		return synctypes.SyncStatusFiltered, currentHash != observedHash
	}

	return synctypes.SyncStatusFiltered, true
}

func initialFilterGeneration(item *synctypes.ObservedItem) int64 {
	if item == nil || !item.Filtered {
		return 0
	}

	if item.FilterGeneration > 0 {
		return item.FilterGeneration
	}

	return 1
}

// computeDeleted handles the "deleted" column of the decision matrix.
func computeDeleted(currentStatus synctypes.SyncStatus) (synctypes.SyncStatus, bool) {
	switch currentStatus {
	case synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloading, synctypes.SyncStatusDownloadFailed, synctypes.SyncStatusSynced:
		return synctypes.SyncStatusPendingDelete, true
	case synctypes.SyncStatusPendingDelete:
		return synctypes.SyncStatusPendingDelete, false // already pending delete
	case synctypes.SyncStatusDeleting:
		return synctypes.SyncStatusDeleting, false // let worker finish
	case synctypes.SyncStatusDeleteFailed:
		return synctypes.SyncStatusPendingDelete, true // retry
	case synctypes.SyncStatusDeleted:
		return synctypes.SyncStatusDeleted, false // already deleted
	case synctypes.SyncStatusFiltered:
		return synctypes.SyncStatusDeleted, true
	default:
		return currentStatus, false
	}
}

// computeSameHash handles the "same hash, not deleted" column.
func computeSameHash(currentStatus synctypes.SyncStatus) (synctypes.SyncStatus, bool) {
	switch currentStatus {
	case synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloading:
		return currentStatus, false // no change / let worker finish
	case synctypes.SyncStatusDownloadFailed:
		return synctypes.SyncStatusPendingDownload, true // retry
	case synctypes.SyncStatusSynced:
		// Critical: prevents re-download on delta redelivery.
		return synctypes.SyncStatusSynced, false
	case synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed, synctypes.SyncStatusDeleted:
		return synctypes.SyncStatusPendingDownload, true // restored/recreated
	case synctypes.SyncStatusFiltered:
		return synctypes.SyncStatusSynced, true
	default:
		return currentStatus, false
	}
}

// computeDifferentHash handles the "different hash, not deleted" column.
func computeDifferentHash(currentStatus synctypes.SyncStatus) (synctypes.SyncStatus, bool) {
	switch currentStatus {
	case synctypes.SyncStatusPendingDownload:
		return synctypes.SyncStatusPendingDownload, true // update hash (still pending)
	case synctypes.SyncStatusDownloading:
		return synctypes.SyncStatusPendingDownload, true // cancel + re-queue
	case synctypes.SyncStatusDownloadFailed:
		return synctypes.SyncStatusPendingDownload, true // new version
	case synctypes.SyncStatusSynced:
		return synctypes.SyncStatusPendingDownload, true
	case synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed, synctypes.SyncStatusDeleted:
		return synctypes.SyncStatusPendingDownload, true // restored+changed / recreated
	case synctypes.SyncStatusFiltered:
		return synctypes.SyncStatusPendingDownload, true // re-entered scope
	default:
		return currentStatus, false
	}
}
