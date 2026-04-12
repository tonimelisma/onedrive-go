package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

// Validates: R-2.4.5
func TestApplyScopeState_MarksOutOfScopeRowsFiltered(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "keep-item",
			ParentID: "root",
			Path:     "keep.txt",
			ItemType: ItemTypeFile,
			Hash:     "keep-hash",
		},
		{
			DriveID:  driveID,
			ItemID:   "drop-item",
			ParentID: "root",
			Path:     "drop.txt",
			ItemType: ItemTypeFile,
			Hash:     "drop-hash",
		},
	}, "", driveID))

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths: []string{"keep.txt"},
	}, nil)
	require.NoError(t, err)

	snapshotJSON, err := syncscope.MarshalSnapshot(snapshot)
	require.NoError(t, err)

	require.NoError(t, mgr.ApplyScopeState(ctx, ScopeStateApplyRequest{
		State: ScopeStateRecord{
			Generation:            1,
			EffectiveSnapshotJSON: snapshotJSON,
			ObservationMode:       ScopeObservationScopedDelta,
			LastReconcileKind:     ScopeReconcileNone,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))

	keepRow := readRemoteStateRow(t, mgr.DB(), "keep-item")
	require.NotNil(t, keepRow)
	assert.False(t, keepRow.IsFiltered)

	dropRow := readRemoteStateRow(t, mgr.DB(), "drop-item")
	require.NotNil(t, dropRow)
	assert.True(t, dropRow.IsFiltered)
	assert.Equal(t, int64(1), dropRow.FilterGeneration)
	assert.Equal(t, RemoteFilterPathScope, dropRow.FilterReason)
}

// Validates: R-2.4.4, R-2.4.5
func TestUpsertSyncMetadataEntries_WritesSortedAndUpdates(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertSyncMetadataEntries(ctx, map[string]string{
		"zeta":  "three",
		"alpha": "one",
	}))
	require.NoError(t, mgr.UpsertSyncMetadataEntries(ctx, map[string]string{
		"alpha": "updated",
		"beta":  "two",
	}))

	rows, err := mgr.DB().QueryContext(ctx, `SELECT key, value FROM sync_metadata ORDER BY key`)
	require.NoError(t, err)
	defer rows.Close()

	var entries []struct {
		Key   string
		Value string
	}
	for rows.Next() {
		var entry struct {
			Key   string
			Value string
		}
		require.NoError(t, rows.Scan(&entry.Key, &entry.Value))
		entries = append(entries, entry)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []struct {
		Key   string
		Value string
	}{
		{Key: "alpha", Value: "updated"},
		{Key: "beta", Value: "two"},
		{Key: "zeta", Value: "three"},
	}, entries)
}

// Validates: R-2.4.4, R-2.4.5
func TestReadScopeState_ReturnsPersistedRecord(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	record, found, err := mgr.ReadScopeState(ctx)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, ScopeStateRecord{}, record)

	require.NoError(t, mgr.ApplyScopeState(ctx, ScopeStateApplyRequest{
		State: ScopeStateRecord{
			Generation:            7,
			EffectiveSnapshotJSON: `{"version":1,"sync_paths":["Docs"]}`,
			ObservationPlanHash:   "scope-hash",
			ObservationMode:       ScopeObservationScopedDelta,
			WebsocketEnabled:      true,
			PendingReentry:        true,
			LastReconcileKind:     ScopeReconcileEnteredPath,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))

	record, found, err = mgr.ReadScopeState(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(7), record.Generation)
	assert.JSONEq(t, `{"version":1,"sync_paths":["Docs"]}`, record.EffectiveSnapshotJSON)
	assert.Equal(t, "scope-hash", record.ObservationPlanHash)
	assert.Equal(t, ScopeObservationScopedDelta, record.ObservationMode)
	assert.True(t, record.WebsocketEnabled)
	assert.True(t, record.PendingReentry)
	assert.Equal(t, ScopeReconcileEnteredPath, record.LastReconcileKind)
	assert.NotZero(t, record.UpdatedAt)
}

// Validates: R-2.4.4, R-2.4.5
func TestNewSyncStore_RepairsScopeStateDriftOnOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	store, err := NewSyncStore(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "filtered-item",
			ParentID: "root",
			Path:     "drop.txt",
			ItemType: ItemTypeFile,
			Hash:     "drop-hash",
			Filtered: true,
		},
	}, "", driveID))

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO scope_state (
			singleton, generation, effective_snapshot_json, observation_plan_hash,
			observation_mode, websocket_enabled, pending_reentry,
			last_reconcile_kind, updated_at
		) VALUES (1, 7, '{broken-json', 'broken', 'scoped_delta', 0, 0, 'none', ?)`,
		time.Now().UnixNano(),
	)
	require.NoError(t, err)
	require.NoError(t, store.Close(context.Background()))

	reopened, err := NewSyncStore(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	row := readRemoteStateRow(t, reopened.DB(), "filtered-item")
	require.NotNil(t, row)
	assert.False(t, row.IsFiltered)
	assert.Equal(t, int64(0), row.FilterGeneration)
	assert.Equal(t, RemoteFilterNone, row.FilterReason)

	scopeState, found, err := reopened.ReadScopeState(ctx)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, ScopeStateRecord{}, scopeState)
}

// Validates: R-2.4.4, R-2.4.5
func TestRepairIntegritySafe_ReconcilesFilteredRowsToPersistedScopeState(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "keep-item",
			ParentID: "root",
			Path:     "keep.txt",
			ItemType: ItemTypeFile,
			Hash:     "keep-hash",
		},
		{
			DriveID:  driveID,
			ItemID:   "drop-item",
			ParentID: "root",
			Path:     "drop.txt",
			ItemType: ItemTypeFile,
			Hash:     "drop-hash",
			Filtered: true,
		},
	}, "", driveID))

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths: []string{"keep.txt"},
	}, nil)
	require.NoError(t, err)

	snapshotJSON, err := syncscope.MarshalSnapshot(snapshot)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx, `
		INSERT INTO scope_state (
			singleton, generation, effective_snapshot_json, observation_plan_hash,
			observation_mode, websocket_enabled, pending_reentry,
			last_reconcile_kind, updated_at
		) VALUES (1, 9, ?, 'hash', 'scoped_delta', 0, 0, 'none', ?)`,
		snapshotJSON,
		time.Now().UnixNano(),
	)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx, `
		UPDATE remote_state
		SET is_filtered = 1, filter_generation = 1, filter_reason = ?
		WHERE item_id = ?`,
		RemoteFilterMarkerScope,
		"drop-item",
	)
	require.NoError(t, err)

	repairs, err := mgr.RepairIntegritySafe(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, repairs)

	dropRow := readRemoteStateRow(t, mgr.DB(), "drop-item")
	require.NotNil(t, dropRow)
	assert.True(t, dropRow.IsFiltered)
	assert.Equal(t, int64(9), dropRow.FilterGeneration)
	assert.Equal(t, RemoteFilterPathScope, dropRow.FilterReason)

	keepRow := readRemoteStateRow(t, mgr.DB(), "keep-item")
	require.NotNil(t, keepRow)
	assert.False(t, keepRow.IsFiltered)
}

// Validates: R-2.4.4, R-2.4.5
func TestRepairIntegritySafe_ReactivatesFilteredRowsWhenPersistedScopeIncludesPath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "reenter-item",
			ParentID: "root",
			Path:     "drop.txt",
			ItemType: ItemTypeFile,
			Hash:     "drop-hash",
			Filtered: true,
		},
	}, "", driveID))

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{}, nil)
	require.NoError(t, err)

	snapshotJSON, err := syncscope.MarshalSnapshot(snapshot)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx, `
		INSERT INTO scope_state (
			singleton, generation, effective_snapshot_json, observation_plan_hash,
			observation_mode, websocket_enabled, pending_reentry,
			last_reconcile_kind, updated_at
		) VALUES (1, 5, ?, 'hash', 'root_delta', 1, 1, ?, ?)`,
		snapshotJSON,
		ScopeReconcileEnteredPath,
		time.Now().UnixNano(),
	)
	require.NoError(t, err)

	repairs, err := mgr.RepairIntegritySafe(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, repairs)

	row := readRemoteStateRow(t, mgr.DB(), "reenter-item")
	require.NotNil(t, row)
	assert.False(t, row.IsFiltered)
	assert.Equal(t, int64(0), row.FilterGeneration)
	assert.Equal(t, RemoteFilterNone, row.FilterReason)
}

// Validates: R-2.4.4, R-2.4.5
func TestNewSyncStore_PendingReentrySurvivesValidRestart(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := t.Context()

	store, err := NewSyncStore(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)

	require.NoError(t, store.ApplyScopeState(ctx, ScopeStateApplyRequest{
		State: ScopeStateRecord{
			Generation:            11,
			EffectiveSnapshotJSON: `{"version":1}`,
			ObservationPlanHash:   "hash",
			ObservationMode:       ScopeObservationRootDelta,
			WebsocketEnabled:      true,
			PendingReentry:        true,
			LastReconcileKind:     ScopeReconcileEnteredPath,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))
	require.NoError(t, store.Close(context.Background()))

	reopened, err := NewSyncStore(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	record, found, err := reopened.ReadScopeState(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(11), record.Generation)
	assert.True(t, record.PendingReentry)
	assert.Equal(t, ScopeReconcileEnteredPath, record.LastReconcileKind)
}
