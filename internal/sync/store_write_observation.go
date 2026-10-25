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
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	sqlGetRemoteStateRow = `SELECT ` + sqlSelectRemoteStateCols + `
		FROM remote_state WHERE item_id = ?`

	sqlInsertRemoteState = `INSERT INTO remote_state
		(drive_id, item_id, path, item_type, hash, size, mtime, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpdateRemoteState = `UPDATE remote_state SET
		drive_id = ?, path = ?, item_type = ?, hash = ?, size = ?, mtime = ?, etag = ?
		WHERE item_id = ?`
)

// CommitObservation atomically persists observed remote mirror state and may
// also advance the primary observation cursor in the same transaction.
func (s *SyncStore) CommitObservation(ctx context.Context, events []observedItem, newToken string, driveID driveid.ID) error {
	return s.commitObservation(ctx, events, newToken, driveID)
}

func (s *SyncStore) commitObservation(
	ctx context.Context,
	events []observedItem,
	newToken string,
	driveID driveid.ID,
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning observation transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation transaction")
	}()

	if commitErr := s.commitObservationTx(ctx, tx, events, newToken, driveID); commitErr != nil {
		err = commitErr
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}

	s.logger.Debug("observations committed",
		slog.Int("items", len(events)),
		slog.String("drive_id", driveID.String()),
	)

	return nil
}

func (s *SyncStore) commitObservationTx(
	ctx context.Context,
	tx sqlTxRunner,
	events []observedItem,
	newToken string,
	driveID driveid.ID,
) error {
	state, err := s.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}
	if driveID.IsZero() {
		driveID = state.ContentDriveID
	}

	for i := range events {
		if events[i].DriveID.IsZero() && !driveID.IsZero() {
			events[i].DriveID = driveID
		}
		if processErr := s.processObservedItem(ctx, tx, &events[i]); processErr != nil {
			return processErr
		}
	}

	if newToken != "" {
		state.Cursor = newToken
		if saveErr := s.writeObservationStateTx(ctx, tx, state); saveErr != nil {
			return saveErr
		}
	}
	return nil
}

func (s *SyncStore) processObservedItem(ctx context.Context, tx sqlTxRunner, item *observedItem) error {
	existing := s.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		if item.IsDeleted {
			return nil
		}
		return s.insertRemoteState(ctx, tx, item)
	}

	if item.IsDeleted {
		if err := deleteObservedRemoteState(ctx, tx, item, existing.Path); err != nil {
			return err
		}
		if existing.ItemType == ItemTypeFolder {
			// Delta reports a deleted folder as one item, like a moved one.
			// Left behind, a child that was never downloaded stayed in
			// remote_state forever: status showed "Remote changes: 1 item"
			// and "Syncing" for a folder that no longer existed.
			return deleteRemoteStateSubtreeTx(ctx, tx, existing.Path)
		}
		return nil
	}

	changed := observedRemoteStateUpdate(existing, item)
	if !changed {
		return nil
	}

	if err := s.updateRemoteStateFromObs(ctx, tx, item); err != nil {
		return err
	}

	// Delta reports a renamed or moved folder as one item; its unchanged
	// descendants are not repeated. Their rows must follow the folder, or
	// every child keeps its old path: once the move is applied locally the
	// children look moved back on OneDrive, and sync moved them back into a
	// re-created old folder.
	if existing.ItemType == ItemTypeFolder && item.Path != "" && item.Path != existing.Path {
		return repathSubtreeTx(ctx, tx, "remote_state", existing.Path, item.Path)
	}

	return nil
}

func deleteObservedRemoteState(
	ctx context.Context,
	tx sqlTxRunner,
	item *observedItem,
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

// deleteRemoteStateSubtreeTx removes every remote_state row strictly below
// folderPath.
func deleteRemoteStateSubtreeTx(ctx context.Context, tx sqlTxRunner, folderPath string) error {
	descendantPrefix := folderPath + "/"
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM remote_state WHERE substr(path, 1, length(?)) = ? COLLATE BINARY`,
		descendantPrefix, descendantPrefix,
	); err != nil {
		return fmt.Errorf("sync: deleting remote_state below deleted folder %s: %w", folderPath, err)
	}

	return nil
}

func observedRemoteStateUpdate(
	existing *remoteStateRow,
	item *observedItem,
) bool {
	pathChanged := item.Path != "" && item.Path != existing.Path
	driveChanged := !item.DriveID.IsZero() && item.DriveID != existing.DriveID
	return pathChanged ||
		driveChanged ||
		item.ItemType != existing.ItemType ||
		item.Hash != existing.Hash ||
		item.Size != existing.Size ||
		item.Mtime != existing.Mtime ||
		item.ETag != existing.ETag
}

func (s *SyncStore) scanRemoteStateRow(ctx context.Context, tx sqlTxRunner, driveID, itemID string) *remoteStateRow {
	contentDriveID := driveid.New(driveID)
	row, err := scanRemoteStateRowWithQuerier(
		contentDriveID,
		func(dest ...any) error {
			return tx.QueryRowContext(ctx, sqlGetRemoteStateRow, itemID).Scan(dest...)
		},
	)
	if err != nil {
		return nil
	}

	return row
}

func (s *SyncStore) insertRemoteState(ctx context.Context, tx sqlTxRunner, item *observedItem) error {
	_, err := tx.ExecContext(ctx, sqlInsertRemoteState,
		item.DriveID.String(),
		item.ItemID, item.Path,
		item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: inserting remote_state for %s: %w", item.Path, err)
	}

	return nil
}

func (s *SyncStore) updateRemoteStateFromObs(
	ctx context.Context,
	tx sqlTxRunner,
	item *observedItem,
) error {
	_, err := tx.ExecContext(ctx, sqlUpdateRemoteState,
		item.DriveID.String(),
		item.Path, item.ItemType,
		nullString(item.Hash), nullKnownInt64(item.Size, true), nullOptionalInt64(item.Mtime),
		nullString(item.ETag),
		item.ItemID,
	)
	if err != nil {
		return fmt.Errorf("sync: updating remote_state for %s: %w", item.Path, err)
	}

	return nil
}
