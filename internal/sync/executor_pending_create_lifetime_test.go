package sync

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// adoptTestPlaceholder records the placeholder of a create at doc.txt and
// has observation adopt it, still empty, as synced empty content.
func adoptTestPlaceholder(t *testing.T, store *SyncStore) ObservedItem {
	t.Helper()

	const path = "doc.txt"
	driveID := driveid.New(testDriveID)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: path, ParentID: GraphRootID, CreatedETag: placeholderCreatedETag,
	}))
	placeholder := ObservedItem{
		DriveID: driveID, ItemID: testPlaceholderID, ParentID: GraphRootID, Path: path, ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: placeholderCreatedETag,
	}
	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{placeholder}, "t-adopt", driveID))
	_, _, adopted := readBaselineRow(t, store, testPlaceholderID)
	require.True(t, adopted, "the placeholder is adopted")

	return placeholder
}

const (
	testPlaceholderID      = "placeholder"
	placeholderCreatedETag = `"p,1"`
)
