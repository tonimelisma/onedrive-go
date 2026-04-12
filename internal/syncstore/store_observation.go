// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - CommitObservation:        atomically persist remote mirror state + advance delta token
//   - processObservedItem:      handle single observed item within transaction
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
)

const (
	sqlGetRemoteStateRow = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, is_filtered, observed_at,
		filter_generation, filter_reason
		FROM remote_state WHERE drive_id = ? AND item_id = ?`

	sqlInsertRemoteState = `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
		 previous_path, is_filtered, observed_at, filter_generation, filter_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpdateRemoteState = `UPDATE remote_state SET
		path = ?, parent_id = ?, item_type = ?, hash = ?, size = ?, mtime = ?, etag = ?,
		previous_path = ?, is_filtered = ?, observed_at = ?, filter_generation = ?, filter_reason = ?
		WHERE drive_id = ? AND item_id = ?`
)

// CommitObservation atomically persists observed remote mirror state and
// advances the delta token in a single transaction.
func (m *SyncStore) CommitObservation(ctx context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error {
	return m.commitObservation(ctx, events, newToken, driveID, "")
}

// CommitObservationForScope is the folder-scoped variant of CommitObservation.
func (m *SyncStore) CommitObservationForScope(
	ctx context.Context,
	events []ObservedItem,
	newToken string,
	driveID driveid.ID,
	scopeID string,
) error {
	return m.commitObservation(ctx, events, newToken, driveID, scopeID)
}

func (m *SyncStore) commitObservation(
	ctx context.Context,
	events []ObservedItem,
	newToken string,
	driveID driveid.ID,
	scopeID string,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning observation transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation transaction")
	}()

	now := m.nowFunc().UnixNano()

	for i := range events {
		if processErr := m.processObservedItem(ctx, tx, &events[i], now); processErr != nil {
			return processErr
		}
	}

	if newToken != "" {
		if saveErr := m.saveDeltaToken(ctx, tx, driveID.String(), scopeID, driveID.String(), newToken, now); saveErr != nil {
			return saveErr
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}

	m.logger.Debug("observations committed",
		slog.Int("items", len(events)),
		slog.String("drive_id", driveID.String()),
		slog.String("scope_id", scopeID),
	)

	return nil
}

func (m *SyncStore) processObservedItem(ctx context.Context, tx sqlTxRunner, item *ObservedItem, now int64) error {
	existing := m.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		if item.IsDeleted {
			return nil
		}
		return m.insertRemoteState(ctx, tx, item, now)
	}

	if item.IsDeleted {
		return deleteObservedRemoteState(ctx, tx, item, existing.Path)
	}

	changed, previousPath := observedRemoteStateUpdate(existing, item)
	if !changed {
		return nil
	}

	return m.updateRemoteStateFromObs(ctx, tx, item, previousPath, now)
}

func deleteObservedRemoteState(
	ctx context.Context,
	tx sqlTxRunner,
	item *ObservedItem,
	existingPath string,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		item.DriveID.String(),
		item.ItemID,
	); err != nil {
		return fmt.Errorf("sync: deleting remote_state for %s: %w", existingPath, err)
	}

	return nil
}

func observedRemoteStateUpdate(
	existing *RemoteStateRow,
	item *ObservedItem,
) (changed bool, previousPath string) {
	pathChanged := item.Path != "" && item.Path != existing.Path
	filterGeneration := initialFilterGeneration(item)
	changed = pathChanged ||
		item.ParentID != existing.ParentID ||
		item.ItemType != existing.ItemType ||
		item.Hash != existing.Hash ||
		item.Size != existing.Size ||
		item.Mtime != existing.Mtime ||
		item.ETag != existing.ETag ||
		item.Filtered != existing.IsFiltered ||
		filterGeneration != existing.FilterGeneration ||
		item.FilterReason != existing.FilterReason

	if pathChanged {
		previousPath = existing.Path
	}

	return changed, previousPath
}

func (m *SyncStore) scanRemoteStateRow(ctx context.Context, tx sqlTxRunner, driveID, itemID string) *RemoteStateRow {
	row, err := scanRemoteStateRowWithQuerier(
		func(dest ...any) error {
			return tx.QueryRowContext(ctx, sqlGetRemoteStateRow, driveID, itemID).Scan(dest...)
		},
	)
	if err != nil {
		return nil
	}

	return row
}

func (m *SyncStore) insertRemoteState(ctx context.Context, tx sqlTxRunner, item *ObservedItem, now int64) error {
	_, err := tx.ExecContext(ctx, sqlInsertRemoteState,
		item.DriveID.String(), item.ItemID, item.Path,
		nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		sql.NullString{},
		boolToInt(item.Filtered),
		now,
		initialFilterGeneration(item),
		string(item.FilterReason),
	)
	if err != nil {
		return fmt.Errorf("sync: inserting remote_state for %s: %w", item.Path, err)
	}

	return nil
}

func (m *SyncStore) updateRemoteStateFromObs(
	ctx context.Context,
	tx sqlTxRunner,
	item *ObservedItem,
	previousPath string,
	now int64,
) error {
	filterGeneration := int64(0)
	filterReason := ""
	if item.Filtered {
		filterGeneration = initialFilterGeneration(item)
		filterReason = string(item.FilterReason)
	}

	_, err := tx.ExecContext(ctx, sqlUpdateRemoteState,
		item.Path, nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		nullString(previousPath), boolToInt(item.Filtered), now, filterGeneration, filterReason,
		item.DriveID.String(), item.ItemID,
	)
	if err != nil {
		return fmt.Errorf("sync: updating remote_state for %s: %w", item.Path, err)
	}

	return nil
}

func initialFilterGeneration(item *ObservedItem) int64 {
	if item == nil || !item.Filtered {
		return 0
	}
	if item.FilterGeneration > 0 {
		return item.FilterGeneration
	}
	return 1
}
