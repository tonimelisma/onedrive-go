package syncstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ApplyShortcutTopology persists parent-owned shortcut-root state. Callers run
// this before committing remote observation progress so topology facts replay if
// the parent namespace state cannot be durably accepted.
func (s *SyncStore) ApplyShortcutTopology(ctx context.Context, batch syncshortcut.ShortcutTopologyBatch) (bool, error) {
	if s == nil || !batch.ShouldApply() {
		return false, nil
	}
	current, err := s.ListShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := syncshortcut.PlanShortcutRootTopology(current, batch)
	if !plan.Changed {
		return false, nil
	}
	if err := s.ReplaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SyncStore) MarkShortcutChildFinalDrainReleasePending(
	ctx context.Context,
	ack syncshortcut.ShortcutChildDrainAck,
) (bool, error) {
	if s == nil || ack.Ref.IsZero() {
		return false, nil
	}
	current, err := s.ListShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := syncshortcut.PlanShortcutRootDrainReleasePending(current, ack)
	if !plan.Changed {
		return false, nil
	}
	if err := s.ReplaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SyncStore) AcknowledgeShortcutChildArtifactsPurged(
	ctx context.Context,
	ack syncshortcut.ShortcutChildArtifactCleanupAck,
) (bool, error) {
	if s == nil || ack.Ref.IsZero() {
		return false, nil
	}
	current, err := s.ListShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := syncshortcut.PlanShortcutRootArtifactCleanupAck(current, ack)
	if !plan.Changed {
		return false, nil
	}
	if err := s.ReplaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SyncStore) ShortcutChildWorkSnapshot(
	ctx context.Context,
	namespaceID string,
	parentSyncRoot string,
) (syncshortcut.ShortcutChildWorkSnapshot, error) {
	if s == nil {
		return syncshortcut.ShortcutChildWorkSnapshot{NamespaceID: namespaceID}, nil
	}
	records, err := s.ListShortcutRoots(ctx)
	if err != nil {
		return syncshortcut.ShortcutChildWorkSnapshot{}, err
	}
	return syncshortcut.ShortcutChildWorkSnapshotFromRootsWithParentRoot(namespaceID, parentSyncRoot, syncdomain.ContentFilterConfig{}, records), nil
}

// ListShortcutRoots returns parent-engine-owned shortcut root state.
func (s *SyncStore) ListShortcutRoots(ctx context.Context) ([]syncshortcut.ShortcutRootRecord, error) {
	return queryShortcutRootRecords(ctx, s.db, "sync: querying shortcut_roots", "sync: iterating shortcut_roots")
}

func queryShortcutRootRecords(
	ctx context.Context,
	db *sql.DB,
	queryContext string,
	iterContext string,
) ([]syncshortcut.ShortcutRootRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT namespace_id, binding_item_id, relative_local_path, local_alias,
		       remote_drive_id, remote_item_id, remote_is_folder, state,
		       protected_paths_json, blocked_detail, local_root_device,
		       local_root_inode, local_root_birth_time, local_root_has_identity, waiting_binding_item_id,
		       waiting_relative_local_path, waiting_local_alias,
		       waiting_remote_drive_id, waiting_remote_item_id,
		       waiting_remote_is_folder
		FROM shortcut_roots
		ORDER BY relative_local_path, binding_item_id`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", queryContext, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	records := make([]syncshortcut.ShortcutRootRecord, 0)
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

// ReplaceShortcutRoots atomically replaces the parent shortcut-root table.
func (s *SyncStore) ReplaceShortcutRoots(ctx context.Context, records []syncshortcut.ShortcutRootRecord) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
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
			local_root_inode, local_root_birth_time, local_root_has_identity, waiting_binding_item_id,
			waiting_relative_local_path, waiting_local_alias,
			waiting_remote_drive_id, waiting_remote_item_id,
			waiting_remote_is_folder
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("sync: preparing shortcut_roots upsert: %w", err)
	}
	defer stmt.Close() //nolint:errcheck // statement handle release; the write itself is checked at Exec

	for i := range records {
		record := syncshortcut.NormalizeShortcutRootRecord(&records[i])
		protectedPaths, marshalErr := json.Marshal(record.ProtectedPaths)
		if marshalErr != nil {
			return fmt.Errorf("sync: encoding protected paths for shortcut root %s: %w", record.BindingItemID, marshalErr)
		}
		identity, hasIdentity := shortcutRootIdentitySQL(record.LocalRootIdentity)
		waiting := record.Waiting
		if waiting == nil {
			waiting = &syncshortcut.ShortcutRootReplacement{}
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
			identity.Device,
			identity.Inode,
			identity.BirthTime,
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

func scanShortcutRootRecord(row shortcutRootScanner) (syncshortcut.ShortcutRootRecord, error) {
	var (
		remoteIsFolder        int
		protectedPathsJSON    string
		localRootDevice       uint64
		localRootInode        uint64
		localRootBirthTime    int64
		localRootHasIdentity  int
		waitingRemoteIsFolder int
		state                 string
		record                syncshortcut.ShortcutRootRecord
		waiting               syncshortcut.ShortcutRootReplacement
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
		&localRootBirthTime,
		&localRootHasIdentity,
		&waiting.BindingItemID,
		&waiting.RelativeLocalPath,
		&waiting.LocalAlias,
		&waiting.RemoteDriveID,
		&waiting.RemoteItemID,
		&waitingRemoteIsFolder,
	); err != nil {
		return syncshortcut.ShortcutRootRecord{}, fmt.Errorf("sync: scanning shortcut_roots row: %w", err)
	}
	if err := json.Unmarshal([]byte(protectedPathsJSON), &record.ProtectedPaths); err != nil {
		return syncshortcut.ShortcutRootRecord{}, fmt.Errorf("sync: decoding shortcut root protected paths for %s: %w", record.BindingItemID, err)
	}
	record.RemoteIsFolder = remoteIsFolder != 0
	record.State = syncshortcut.ShortcutRootState(state)
	if localRootHasIdentity != 0 {
		record.LocalRootIdentity = &synctree.FileIdentity{
			Device:    localRootDevice,
			Inode:     localRootInode,
			BirthTime: localRootBirthTime,
		}
	}
	waiting.RemoteIsFolder = waitingRemoteIsFolder != 0
	if waiting.BindingItemID != "" {
		record.Waiting = &waiting
	}
	return syncshortcut.NormalizeShortcutRootRecord(&record), nil
}

func shortcutRootIdentitySQL(identity *synctree.FileIdentity) (synctree.FileIdentity, bool) {
	if identity == nil {
		return synctree.FileIdentity{}, false
	}
	return *identity, true
}
