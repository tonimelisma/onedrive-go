package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.4.5
func TestApplyRemoteScope_MarksOutOfScopeRowsFiltered(t *testing.T) {
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

	require.NoError(t, mgr.ApplyRemoteScope(ctx, snapshot))

	keepRow := readRemoteStateRow(t, mgr.DB(), "keep-item")
	require.NotNil(t, keepRow)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, keepRow.SyncStatus)

	dropRow := readRemoteStateRow(t, mgr.DB(), "drop-item")
	require.NotNil(t, dropRow)
	assert.Equal(t, synctypes.SyncStatusFiltered, dropRow.SyncStatus)
}
