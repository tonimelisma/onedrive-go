// Package sync persists sync baseline, observation, failure, scope-block, and metadata state.
//
// Contents:
//   - CommitObservation:        atomically persist remote mirror state + advance observation cursor
//   - processObservedItem:      handle single observed item within transaction
//   - scanRemoteStateRow:       read one remote_state row
//   - insertRemoteState:        insert new remote_state row
//   - updateRemoteStateFromObs: update existing remote_state from observation
//
// Related files:
//   - store.go:            SyncStore type definition and lifecycle
//   - store_observation_state.go: observation state helpers
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	slashpath "path"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	sqlGetRemoteStateRow = `SELECT item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path
		FROM remote_state WHERE item_id = ?`

	sqlInsertRemoteState = `INSERT INTO remote_state
		(item_id, path, parent_id, item_type, hash, size, mtime, etag,
		 previous_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpdateRemoteState = `UPDATE remote_state SET
		path = ?, parent_id = ?, item_type = ?, hash = ?, size = ?, mtime = ?, etag = ?,
		previous_path = ?
		WHERE item_id = ?`
)

// CommitObservation atomically persists observed remote mirror state and may
// also advance the primary observation cursor in the same transaction.
func (m *SyncStore) CommitObservation(ctx context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error {
	return m.commitObservation(ctx, events, newToken, driveID)
}

func (m *SyncStore) commitObservation(
	ctx context.Context,
	events []ObservedItem,
	newToken string,
	driveID driveid.ID,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning observation transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation transaction")
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ConfiguredDriveID.IsZero() {
		driveID = state.ConfiguredDriveID
	}

	for i := range events {
		if !driveID.IsZero() {
			events[i].DriveID = driveID
		}
		if !events[i].IsDeleted && IsAlwaysExcluded(slashpath.Base(events[i].Path)) {
			continue
		}
		if processErr := m.processObservedItem(ctx, tx, &events[i]); processErr != nil {
			return processErr
		}
	}

	if newToken != "" {
		state.Cursor = newToken
		if saveErr := m.writeObservationStateTx(ctx, tx, state); saveErr != nil {
			return saveErr
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}

	m.logger.Debug("observations committed",
		slog.Int("items", len(events)),
		slog.String("drive_id", driveID.String()),
	)

	return nil
}

func (m *SyncStore) processObservedItem(ctx context.Context, tx sqlTxRunner, item *ObservedItem) error {
	existing := m.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		if item.IsDeleted {
			return nil
		}
		return m.insertRemoteState(ctx, tx, item)
	}

	if item.IsDeleted {
		return deleteObservedRemoteState(ctx, tx, item, existing.Path)
	}

	changed, previousPath := observedRemoteStateUpdate(existing, item)
	if !changed {
		return nil
	}

	return m.updateRemoteStateFromObs(ctx, tx, item, previousPath)
}

func deleteObservedRemoteState(
	ctx context.Context,
	tx sqlTxRunner,
	item *ObservedItem,
	existingPath string,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM remote_state WHERE item_id = ?`,
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
	changed = pathChanged ||
		item.ParentID != existing.ParentID ||
		item.ItemType != existing.ItemType ||
		item.Hash != existing.Hash ||
		item.Size != existing.Size ||
		item.Mtime != existing.Mtime ||
		item.ETag != existing.ETag

	if pathChanged {
		previousPath = existing.Path
	}

	return changed, previousPath
}

func (m *SyncStore) scanRemoteStateRow(ctx context.Context, tx sqlTxRunner, driveID, itemID string) *RemoteStateRow {
	configuredDriveID := driveid.New(driveID)
	row, err := scanRemoteStateRowWithQuerier(
		configuredDriveID,
		func(dest ...any) error {
			return tx.QueryRowContext(ctx, sqlGetRemoteStateRow, itemID).Scan(dest...)
		},
	)
	if err != nil {
		return nil
	}

	return row
}

func (m *SyncStore) insertRemoteState(ctx context.Context, tx sqlTxRunner, item *ObservedItem) error {
	_, err := tx.ExecContext(ctx, sqlInsertRemoteState,
		item.ItemID, item.Path,
		nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		sql.NullString{},
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
) error {
	_, err := tx.ExecContext(ctx, sqlUpdateRemoteState,
		item.Path, nullString(item.ParentID), item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		nullString(previousPath),
		item.ItemID,
	)
	if err != nil {
		return fmt.Errorf("sync: updating remote_state for %s: %w", item.Path, err)
	}

	return nil
}
