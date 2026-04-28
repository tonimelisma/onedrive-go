package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// applyShortcutTopology persists parent-owned shortcut-root state. Callers run
// this before committing remote observation progress so topology facts replay if
// the parent namespace state cannot be durably accepted.
func (m *SyncStore) applyShortcutTopology(ctx context.Context, batch shortcutTopologyBatch) (bool, error) {
	if m == nil || !batch.shouldApply() {
		return false, nil
	}
	current, err := m.listShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := planShortcutRootTopology(current, batch)
	if !plan.Changed {
		return false, nil
	}
	if err := m.replaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SyncStore) markShortcutChildFinalDrainReleasePending(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (bool, error) {
	if m == nil || ack.Ref.IsZero() {
		return false, nil
	}
	current, err := m.listShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := planShortcutRootDrainReleasePending(current, ack)
	if !plan.Changed {
		return false, nil
	}
	if err := m.replaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SyncStore) acknowledgeShortcutChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (bool, error) {
	if m == nil || ack.Ref.IsZero() {
		return false, nil
	}
	current, err := m.listShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := planShortcutRootArtifactCleanupAck(current, ack)
	if !plan.Changed {
		return false, nil
	}
	if err := m.replaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SyncStore) ShortcutChildWorkSnapshot(
	ctx context.Context,
	namespaceID string,
	parentSyncRoot string,
) (ShortcutChildWorkSnapshot, error) {
	if m == nil {
		return ShortcutChildWorkSnapshot{NamespaceID: namespaceID}, nil
	}
	records, err := m.listShortcutRoots(ctx)
	if err != nil {
		return ShortcutChildWorkSnapshot{}, err
	}
	return shortcutChildWorkSnapshotFromRootsWithParentRoot(namespaceID, parentSyncRoot, records), nil
}

// listShortcutRoots returns parent-engine-owned shortcut root state.
func (m *SyncStore) listShortcutRoots(ctx context.Context) ([]ShortcutRootRecord, error) {
	return queryShortcutRootRecords(ctx, m.db, "sync: querying shortcut_roots", "sync: iterating shortcut_roots")
}

func queryShortcutRootRecords(
	ctx context.Context,
	db *sql.DB,
	queryContext string,
	iterContext string,
) ([]ShortcutRootRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT namespace_id, binding_item_id, relative_local_path, local_alias,
		       remote_drive_id, remote_item_id, remote_is_folder, state,
		       protected_paths_json, blocked_detail, local_root_device,
		       local_root_inode, local_root_has_identity, waiting_binding_item_id,
		       waiting_relative_local_path, waiting_local_alias,
		       waiting_remote_drive_id, waiting_remote_item_id,
		       waiting_remote_is_folder
		FROM shortcut_roots
		ORDER BY relative_local_path, binding_item_id`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", queryContext, err)
	}
	defer rows.Close()

	records := make([]ShortcutRootRecord, 0)
	for rows.Next() {
		record, scanErr := scanShortcutRootRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterContext, err)
	}
	return records, nil
}

// replaceShortcutRoots atomically replaces the parent shortcut-root table.
func (m *SyncStore) replaceShortcutRoots(ctx context.Context, records []ShortcutRootRecord) (err error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning shortcut_roots replacement: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback shortcut_roots replacement")
	}()

	if _, err = tx.ExecContext(ctx, `DELETE FROM shortcut_roots`); err != nil {
		return fmt.Errorf("sync: clearing shortcut_roots: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO shortcut_roots (
			namespace_id, binding_item_id, relative_local_path, local_alias,
			remote_drive_id, remote_item_id, remote_is_folder, state,
			protected_paths_json, blocked_detail, local_root_device,
			local_root_inode, local_root_has_identity, waiting_binding_item_id,
			waiting_relative_local_path, waiting_local_alias,
			waiting_remote_drive_id, waiting_remote_item_id,
			waiting_remote_is_folder
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("sync: preparing shortcut_roots upsert: %w", err)
	}
	defer stmt.Close()

	for i := range records {
		record := normalizeShortcutRootRecord(&records[i])
		protectedPaths, marshalErr := json.Marshal(record.ProtectedPaths)
		if marshalErr != nil {
			return fmt.Errorf("sync: encoding protected paths for shortcut root %s: %w", record.BindingItemID, marshalErr)
		}
		device, inode, hasIdentity := shortcutRootIdentitySQL(record.LocalRootIdentity)
		waiting := record.Waiting
		if waiting == nil {
			waiting = &ShortcutRootReplacement{}
		}
		if _, err = stmt.ExecContext(ctx,
			record.NamespaceID,
			record.BindingItemID,
			record.RelativeLocalPath,
			record.LocalAlias,
			record.RemoteDriveID.String(),
			record.RemoteItemID,
			boolInt(record.RemoteIsFolder),
			string(record.State),
			string(protectedPaths),
			record.BlockedDetail,
			device,
			inode,
			boolInt(hasIdentity),
			waiting.BindingItemID,
			waiting.RelativeLocalPath,
			waiting.LocalAlias,
			waiting.RemoteDriveID.String(),
			waiting.RemoteItemID,
			boolInt(waiting.RemoteIsFolder),
		); err != nil {
			return fmt.Errorf("sync: inserting shortcut root %s: %w", record.BindingItemID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing shortcut_roots replacement: %w", err)
	}
	return nil
}

type shortcutRootScanner interface {
	Scan(dest ...any) error
}

func scanShortcutRootRecord(row shortcutRootScanner) (ShortcutRootRecord, error) {
	var (
		remoteIsFolder        int
		protectedPathsJSON    string
		localRootDevice       uint64
		localRootInode        uint64
		localRootHasIdentity  int
		waitingRemoteIsFolder int
		state                 string
		record                ShortcutRootRecord
		waiting               ShortcutRootReplacement
	)
	if err := row.Scan(
		&record.NamespaceID,
		&record.BindingItemID,
		&record.RelativeLocalPath,
		&record.LocalAlias,
		&record.RemoteDriveID,
		&record.RemoteItemID,
		&remoteIsFolder,
		&state,
		&protectedPathsJSON,
		&record.BlockedDetail,
		&localRootDevice,
		&localRootInode,
		&localRootHasIdentity,
		&waiting.BindingItemID,
		&waiting.RelativeLocalPath,
		&waiting.LocalAlias,
		&waiting.RemoteDriveID,
		&waiting.RemoteItemID,
		&waitingRemoteIsFolder,
	); err != nil {
		return ShortcutRootRecord{}, fmt.Errorf("sync: scanning shortcut_roots row: %w", err)
	}
	if err := json.Unmarshal([]byte(protectedPathsJSON), &record.ProtectedPaths); err != nil {
		return ShortcutRootRecord{}, fmt.Errorf("sync: decoding shortcut root protected paths for %s: %w", record.BindingItemID, err)
	}
	record.RemoteIsFolder = remoteIsFolder != 0
	record.State = ShortcutRootState(state)
	if localRootHasIdentity != 0 {
		record.LocalRootIdentity = &synctree.FileIdentity{Device: localRootDevice, Inode: localRootInode}
	}
	waiting.RemoteIsFolder = waitingRemoteIsFolder != 0
	if waiting.BindingItemID != "" {
		record.Waiting = &waiting
	}
	return normalizeShortcutRootRecord(&record), nil
}

func shortcutRootIdentitySQL(identity *synctree.FileIdentity) (uint64, uint64, bool) {
	if identity == nil {
		return 0, 0, false
	}
	return identity.Device, identity.Inode, true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
