package syncstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.4.5
func TestApplyScopeState_MarksOutOfScopeRowsFiltered(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "keep-item",
			ParentID: "root",
			Path:     "keep.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "keep-hash",
		},
		{
			DriveID:  driveID,
			ItemID:   "drop-item",
			ParentID: "root",
			Path:     "drop.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "drop-hash",
		},
	}, "", driveID))

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths: []string{"keep.txt"},
	}, nil)
	require.NoError(t, err)

	snapshotJSON, err := syncscope.MarshalSnapshot(snapshot)
	require.NoError(t, err)

	require.NoError(t, mgr.ApplyScopeState(ctx, synctypes.ScopeStateApplyRequest{
		State: synctypes.ScopeStateRecord{
			Generation:            1,
			EffectiveSnapshotJSON: snapshotJSON,
			ObservationMode:       synctypes.ScopeObservationScopedDelta,
			LastReconcileKind:     synctypes.ScopeReconcileNone,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))

	keepRow := readRemoteStateRow(t, mgr.DB(), "keep-item")
	require.NotNil(t, keepRow)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, keepRow.SyncStatus)

	dropRow := readRemoteStateRow(t, mgr.DB(), "drop-item")
	require.NotNil(t, dropRow)
	assert.Equal(t, synctypes.SyncStatusFiltered, dropRow.SyncStatus)
	assert.Equal(t, int64(1), dropRow.FilterGeneration)
	assert.Equal(t, synctypes.RemoteFilterPathScope, dropRow.FilterReason)
}

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
	assert.Equal(t, synctypes.ScopeStateRecord{}, record)

	require.NoError(t, mgr.ApplyScopeState(ctx, synctypes.ScopeStateApplyRequest{
		State: synctypes.ScopeStateRecord{
			Generation:            7,
			EffectiveSnapshotJSON: `{"version":1,"sync_paths":["Docs"]}`,
			ObservationPlanHash:   "scope-hash",
			ObservationMode:       synctypes.ScopeObservationScopedDelta,
			WebsocketEnabled:      true,
			PendingReentry:        true,
			LastReconcileKind:     synctypes.ScopeReconcileEnteredPath,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))

	record, found, err = mgr.ReadScopeState(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(7), record.Generation)
	assert.JSONEq(t, `{"version":1,"sync_paths":["Docs"]}`, record.EffectiveSnapshotJSON)
	assert.Equal(t, "scope-hash", record.ObservationPlanHash)
	assert.Equal(t, synctypes.ScopeObservationScopedDelta, record.ObservationMode)
	assert.True(t, record.WebsocketEnabled)
	assert.True(t, record.PendingReentry)
	assert.Equal(t, synctypes.ScopeReconcileEnteredPath, record.LastReconcileKind)
	assert.NotZero(t, record.UpdatedAt)
}

func TestReactivatedRemoteStatus(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		path          string
		hash          string
		baselineFound bool
		baselinePath  string
		baselineHash  string
		want          synctypes.SyncStatus
	}{
		{
			name:          "baseline exact match returns synced",
			path:          "docs/file.txt",
			hash:          "hash-a",
			baselineFound: true,
			baselinePath:  "docs/file.txt",
			baselineHash:  "hash-a",
			want:          synctypes.SyncStatusSynced,
		},
		{
			name:          "path mismatch returns pending download",
			path:          "docs/file.txt",
			hash:          "hash-a",
			baselineFound: true,
			baselinePath:  "docs/other.txt",
			baselineHash:  "hash-a",
			want:          synctypes.SyncStatusPendingDownload,
		},
		{
			name:          "hash mismatch returns pending download",
			path:          "docs/file.txt",
			hash:          "hash-a",
			baselineFound: true,
			baselinePath:  "docs/file.txt",
			baselineHash:  "hash-b",
			want:          synctypes.SyncStatusPendingDownload,
		},
		{
			name:          "missing baseline returns pending download",
			path:          "docs/file.txt",
			hash:          "hash-a",
			baselineFound: false,
			want:          synctypes.SyncStatusPendingDownload,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, reactivatedRemoteStatus(
				tc.path,
				tc.hash,
				tc.baselineFound,
				tc.baselinePath,
				tc.baselineHash,
			))
		})
	}
}
