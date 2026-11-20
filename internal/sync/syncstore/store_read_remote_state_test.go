package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.2
func TestSyncStore_GetRemoteStateByID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-1",
			Path:     "docs/report.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-1",
			Size:     42,
			Mtime:    1234,
			ETag:     "etag-1",
		},
	}, "delta-token", driveID))

	row, found, err := store.GetRemoteStateByID(ctx, driveID, "item-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, driveID, row.DriveID)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.Equal(t, "hash-1", row.Hash)
	assert.Equal(t, int64(42), row.Size)
	assert.Equal(t, int64(1234), row.Mtime)
	assert.Equal(t, "etag-1", row.ETag)

	missing, found, err := store.GetRemoteStateByID(ctx, driveID, "missing")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, missing)
}
