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
			Path:     "docs/b.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-b",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-a",
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

// Validates: R-2.2
func TestSyncStore_ListRemoteState_PreservesPerRowDriveOwnership(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	contentDriveID := driveid.New("mount-drive")
	sharedDriveID := driveid.New("shared-drive")

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  contentDriveID,
			ItemID:   "item-configured",
			Path:     "docs/configured.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-configured",
		},
		{
			DriveID:  sharedDriveID,
			ItemID:   "item-shared",
			Path:     "Shared/shared.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-shared",
		},
	}, "delta-token", contentDriveID))

	rows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byPath := make(map[string]RemoteStateRow, len(rows))
	for i := range rows {
		byPath[rows[i].Path] = rows[i]
	}

	assert.Equal(t, contentDriveID, byPath["docs/configured.txt"].DriveID)
	assert.Equal(t, sharedDriveID, byPath["Shared/shared.txt"].DriveID)
}

// Validates: R-2.2
func TestSyncStore_GetRemoteStateByPath_RejectsMismatchedDriveWhenStateAlreadyConfigured(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	contentDriveID := driveid.New("mount-drive")
	attemptedDriveID := driveid.New("attempted-drive")

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  contentDriveID,
		ItemID:   "item-a",
		Path:     "docs/a.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-a",
	}}, "delta-token", contentDriveID))

	row, found, err := store.GetRemoteStateByPath(ctx, "docs/a.txt", attemptedDriveID)
	require.Error(t, err)
	assert.Nil(t, row)
	assert.False(t, found)
	assert.Contains(t, err.Error(), "state DB content drive mismatch")
}

// Validates: R-2.2
func TestSyncStore_CommitObservation_UpdatesPerRowDriveOwnershipWithoutOtherMetadataChanges(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	contentDriveID := driveid.New("mount-drive")
	sharedDriveID := driveid.New("shared-drive")

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		ItemID:   "item-shared",
		Path:     "Shared/shared.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-shared",
		Size:     42,
		Mtime:    1234,
		ETag:     "etag-shared",
	}}, "delta-1", contentDriveID))

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  sharedDriveID,
		ItemID:   "item-shared",
		Path:     "Shared/shared.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-shared",
		Size:     42,
		Mtime:    1234,
		ETag:     "etag-shared",
	}}, "delta-2", contentDriveID))

	row, found, err := store.GetRemoteStateByPath(ctx, "Shared/shared.txt", contentDriveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, sharedDriveID, row.DriveID)
}
