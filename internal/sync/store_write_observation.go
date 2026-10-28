// Package sync persists sync baseline, observation, failure, scope-block, and metadata state.
//
// Contents:
//   - CommitObservation:        atomically persist remote mirror state + advance observation cursor
//   - applyObservationWindowTx: apply one window order-independently (park, delete, place)
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
	"errors"
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

	adopted, commitErr := s.commitObservationTx(ctx, tx, events, newToken, driveID)
	if commitErr != nil {
		err = commitErr
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation transaction: %w", err)
	}
	s.cacheAdoptedPlaceholders(adopted)

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
) ([]*BaselineEntry, error) {
	state, err := s.readObservationStateTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return nil, ensureErr
	}
	if driveID.IsZero() {
		driveID = state.ContentDriveID
	}

	for i := range events {
		if events[i].DriveID.IsZero() && !driveID.IsZero() {
			events[i].DriveID = driveID
		}
	}
	adopted, err := s.applyObservationWindowTx(ctx, tx, events)
	if err != nil {
		return nil, err
	}
	if twinErr := s.reconcileRemoteTwinIssuesTx(ctx, tx); twinErr != nil {
		return nil, twinErr
	}

	if newToken != "" {
		state.Cursor = newToken
		if saveErr := s.writeObservationStateTx(ctx, tx, state); saveErr != nil {
			return nil, saveErr
		}
	}
	return adopted, nil
}

// applyObservationWindowTx applies one observation window as one set of
// facts, so the result does not depend on the order its items arrive in
// (R-2.12.7, D-060). Delta reports a swap, a rename over a name another item
// gives up, or a delete and re-create at one path in any order, and across
// pages; applied item by item, a newcomer could reach a path its occupant had
// not left yet. remote_state.path is UNIQUE, so the newcomer was stored as a
// remote name twin and the user was warned about two items that were really
// one window's moves.
//
//  1. Every live item whose row changes path is parked first, subtree
//     included, at a path no real item can hold, which frees its old path.
//     Parking before the deletes also lifts a child that moved out of a
//     deleted folder clear of the folder's subtree delete.
//  2. Tombstones (and synthesized orphan deletes) are applied.
//  3. Live items are applied in window order and take their final paths.
//     Only a path still held by a row outside this window's moves makes a
//     newcomer a twin, which is then a real one (store_remote_twins.go).
//  4. A parked row the window did not place (a stale placeholder echo) goes
//     back to its old path, and every path freed by parking is offered to a
//     twin shadowed at it.
//
// Only the last observation of an item in the window counts: each one is a
// full snapshot of the item, so an earlier one is superseded, not a step.
func (s *SyncStore) applyObservationWindowTx(
	ctx context.Context, tx sqlTxRunner, events []observedItem,
) ([]*BaselineEntry, error) {
	window := latestObservationPerItem(events)

	parked, err := s.parkMovingRemoteRowsTx(ctx, tx, window)
	if err != nil {
		return nil, err
	}

	var adopted []*BaselineEntry
	for _, deletes := range []bool{true, false} {
		for _, item := range window {
			if item.IsDeleted != deletes {
				continue
			}
			if processErr := s.processObservedItemWithPlaceholders(ctx, tx, item, &adopted); processErr != nil {
				return nil, processErr
			}
		}
	}

	if err := s.restoreParkedRemoteRowsTx(ctx, tx, parked); err != nil {
		return nil, err
	}

	return adopted, nil
}

// latestObservationPerItem returns the window's items with only the last
// observation of each item ID, in the position of that last observation.
func latestObservationPerItem(events []observedItem) []*observedItem {
	last := make(map[string]int, len(events))
	for i := range events {
		last[events[i].ItemID] = i
	}

	window := make([]*observedItem, 0, len(last))
	for i := range events {
		if last[events[i].ItemID] == i {
			window = append(window, &events[i])
		}
	}

	return window
}

// remoteParkMarker starts the stored path of a row parked while one
// observation window is applied. ':' is forbidden in OneDrive names, so no
// real stored path can start with it, and a parked row never outlives the
// transaction that parked it.
const remoteParkMarker = ":park:"

func remoteParkPath(itemID string) string {
	return remoteParkMarker + itemID
}

// parkedRemoteRow is a row moved to its parking path, with the path it held.
type parkedRemoteRow struct {
	itemID  string
	oldPath string
}

// parkMovingRemoteRowsTx parks every live item of the window whose row holds
// a real path other than the one it is reported at. A shadowed twin holds no
// real path, so it has nothing to free and is not parked.
func (s *SyncStore) parkMovingRemoteRowsTx(
	ctx context.Context, tx sqlTxRunner, window []*observedItem,
) ([]parkedRemoteRow, error) {
	var parked []parkedRemoteRow
	for _, item := range window {
		if item.IsDeleted || item.Path == "" {
			continue
		}
		existing := s.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)
		if existing == nil || existing.Path == item.Path || remoteTwinShadowParent(existing.Path) != "" {
			continue
		}
		parkPath := remoteParkPath(item.ItemID)
		if _, err := tx.ExecContext(ctx, `UPDATE remote_state SET path = ? WHERE item_id = ?`, parkPath, item.ItemID); err != nil {
			return nil, fmt.Errorf("sync: parking remote item %s from %s: %w", item.ItemID, existing.Path, err)
		}
		if existing.ItemType == ItemTypeFolder {
			if err := repathSubtreeTx(ctx, tx, "remote_state", existing.Path, parkPath); err != nil {
				return nil, err
			}
			if err := keepRemoteTwinsAtHolderPathTx(ctx, tx, existing.Path, parkPath); err != nil {
				return nil, err
			}
		}
		parked = append(parked, parkedRemoteRow{itemID: item.ItemID, oldPath: existing.Path})
	}

	return parked, nil
}

// restoreParkedRemoteRowsTx returns a parked row the window did not place to
// its old path, then offers every path parking freed to a shadowed twin.
func (s *SyncStore) restoreParkedRemoteRowsTx(ctx context.Context, tx sqlTxRunner, parked []parkedRemoteRow) error {
	for _, p := range parked {
		row := s.scanRemoteStateRow(ctx, tx, "", p.itemID)
		if row == nil || row.Path != remoteParkPath(p.itemID) {
			continue
		}
		back := observedItem{
			DriveID: row.DriveID, ItemID: row.ItemID, Path: p.oldPath, ItemType: row.ItemType,
			Hash: row.Hash, Size: row.Size, Mtime: row.Mtime, ETag: row.ETag,
		}
		if err := s.processObservedItem(ctx, tx, &back); err != nil {
			return err
		}
	}
	for _, p := range parked {
		if err := promoteRemoteTwinTx(ctx, tx, p.oldPath); err != nil {
			return err
		}
	}

	return nil
}

// processObservedItemWithPlaceholders applies pending-create recognition
// (store_pending_creates.go) around processObservedItem.
func (s *SyncStore) processObservedItemWithPlaceholders(
	ctx context.Context, tx sqlTxRunner, item *observedItem, adopted *[]*BaselineEntry,
) error {
	pending, decision, err := classifyObservedPlaceholderTx(ctx, tx, item)
	if err != nil {
		return err
	}

	switch decision {
	case placeholderStale:
		// Our placeholder's state from before the upload committed: the
		// baseline already holds the content. Not a remote change.
		return nil
	case placeholderResolved:
		if err := deletePendingCreateTx(ctx, tx, item.ItemID); err != nil {
			return err
		}
	case placeholderNotPending, placeholderAdopt:
	}

	if err := s.processObservedItem(ctx, tx, item); err != nil {
		return err
	}
	if decision != placeholderAdopt {
		return nil
	}

	return adoptPlaceholderTx(ctx, tx, item, pending, adopted)
}

// cacheAdoptedPlaceholders puts placeholders adopted by an observation commit
// into the in-memory baseline, which CommitMutation otherwise keeps current.
func (s *SyncStore) cacheAdoptedPlaceholders(adopted []*BaselineEntry) {
	if len(adopted) == 0 {
		return
	}

	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()
	if s.baseline == nil {
		return
	}
	for _, entry := range adopted {
		s.baseline.Put(entry)
	}
}

func (s *SyncStore) processObservedItem(ctx context.Context, tx sqlTxRunner, item *observedItem) error {
	existing := s.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ItemID)

	if existing == nil {
		if item.IsDeleted {
			return nil
		}
		return s.insertObservedRemoteState(ctx, tx, item)
	}

	if item.IsDeleted {
		return deleteObservedRemoteStateTree(ctx, tx, item, existing)
	}

	if item.Path != "" {
		stored, err := s.storedPathForObservedItem(ctx, tx, item)
		if err != nil {
			return err
		}
		item.Path = stored
	}

	if !observedRemoteStateUpdate(existing, item) {
		return nil
	}

	if err := s.updateRemoteStateFromObs(ctx, tx, item); err != nil {
		return err
	}

	return followObservedPathChangeTx(ctx, tx, existing, item)
}

// deleteObservedRemoteStateTree removes a deleted item's row and, for a
// folder, its subtree, then hands the freed path to a shadowed twin.
func deleteObservedRemoteStateTree(ctx context.Context, tx sqlTxRunner, item *observedItem, existing *remoteStateRow) error {
	if err := deleteObservedRemoteState(ctx, tx, item, existing.Path); err != nil {
		return err
	}
	if existing.ItemType == ItemTypeFolder {
		// Delta reports a deleted folder as one item, like a moved one.
		// Left behind, a child that was never downloaded stayed in
		// remote_state forever: status showed "Remote changes: 1 item"
		// and "Syncing" for a folder that no longer existed.
		if err := deleteRemoteStateSubtreeTx(ctx, tx, existing.Path); err != nil {
			return err
		}
	}

	return promoteRemoteTwinTx(ctx, tx, existing.Path)
}

// followObservedPathChangeTx moves a renamed or moved folder's subtree with
// it and hands the freed path to a shadowed twin.
//
// Delta reports a renamed or moved folder as one item; its unchanged
// descendants are not repeated. Their rows must follow the folder, or
// every child keeps its old path: once the move is applied locally the
// children look moved back on OneDrive, and sync moved them back into a
// re-created old folder.
func followObservedPathChangeTx(ctx context.Context, tx sqlTxRunner, existing *remoteStateRow, item *observedItem) error {
	if item.Path == "" || item.Path == existing.Path {
		return nil
	}
	if existing.ItemType == ItemTypeFolder {
		if err := repathSubtreeTx(ctx, tx, "remote_state", existing.Path, item.Path); err != nil {
			return err
		}
		if err := keepRemoteTwinsAtHolderPathTx(ctx, tx, existing.Path, item.Path); err != nil {
			return err
		}
	}

	return promoteRemoteTwinTx(ctx, tx, existing.Path)
}

// insertObservedRemoteState inserts a newly observed item at its stored path
// (store_remote_twins.go).
func (s *SyncStore) insertObservedRemoteState(ctx context.Context, tx sqlTxRunner, item *observedItem) error {
	stored, err := s.storedPathForObservedItem(ctx, tx, item)
	if err != nil {
		return err
	}
	item.Path = stored

	return s.insertRemoteState(ctx, tx, item)
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

// ForgetRemoteItem removes itemID, and its subtree when it is a folder, from
// remote_state after a live read proved it gone.
func (s *SyncStore) ForgetRemoteItem(ctx context.Context, itemID string) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning forget-remote-item transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback forget-remote-item transaction")
	}()

	var (
		path     string
		itemType string
	)
	switch scanErr := tx.QueryRowContext(ctx,
		`SELECT path, item_type FROM remote_state WHERE item_id = ?`, itemID,
	).Scan(&path, &itemType); {
	case errors.Is(scanErr, sql.ErrNoRows):
		return nil
	case scanErr != nil:
		return fmt.Errorf("sync: reading remote_state for %s: %w", itemID, scanErr)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM remote_state WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("sync: deleting remote_state for %s: %w", path, err)
	}
	if parsed, parseErr := parseItemType(itemType); parseErr == nil && parsed == ItemTypeFolder {
		if err := deleteRemoteStateSubtreeTx(ctx, tx, path); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing forget-remote-item transaction: %w", err)
	}

	return nil
}

// deleteRemoteStateSubtreeTx removes every remote_state row strictly below
// folderPath.
// deleteRemoteStateSubtreeTx deletes the rows below a deleted folder, except
// remote twins shadowed at the folder's own path: they are its siblings and
// one of them takes the freed path (promoteRemoteTwinTx).
func deleteRemoteStateSubtreeTx(ctx context.Context, tx sqlTxRunner, folderPath string) error {
	descendantPrefix := folderPath + "/"
	twinPrefix := folderPath + "/" + remoteTwinShadowMarker
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM remote_state WHERE substr(path, 1, length(?)) = ? COLLATE BINARY
			AND substr(path, 1, length(?)) != ? COLLATE BINARY`,
		descendantPrefix, descendantPrefix, twinPrefix, twinPrefix,
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
		return fmt.Errorf("sync: inserting remote_state for item %s at %s: %w", item.ItemID, item.Path, err)
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
		return fmt.Errorf("sync: updating remote_state for item %s to %s: %w", item.ItemID, item.Path, err)
	}

	return nil
}
