package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.2
func TestSyncStore_ListRemoteStateAndRejectMismatchedDriveLookup(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-b",
			ParentID: "root",
			Path:     "docs/b.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-b",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-a",
			ParentID: "root",
			Path:     "docs/a.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-a",
		},
	}, "delta-token", driveID))

	rows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, driveID, rows[0].DriveID)
	assert.Equal(t, driveID, rows[1].DriveID)
	assert.ElementsMatch(t, []string{"docs/a.txt", "docs/b.txt"}, []string{rows[0].Path, rows[1].Path})

	row, found, err := store.GetRemoteStateByPath(ctx, "docs/a.txt", driveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "item-a", row.ItemID)
}
