package syncexec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// adoptTestPlaceholder records the placeholder of a create at doc.txt and
// has observation adopt it, still empty, as synced empty content.
func adoptTestPlaceholder(t *testing.T, store *syncstore.SyncStore) syncdomain.ObservedItem {
	t.Helper()

	const path = "doc.txt"
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: path, ParentID: GraphRootID, CreatedETag: placeholderCreatedETag,
	}))
	placeholder := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: testPlaceholderID, ParentID: GraphRootID, Path: path, ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: placeholderCreatedETag,
	}
	require.NoError(t, store.CommitObservation(t.Context(), []syncdomain.ObservedItem{placeholder}, "t-adopt", driveID))
	_, _, adopted := syncstoretest.ReadBaselineRow(t, store, testPlaceholderID)
	require.True(t, adopted, "the placeholder is adopted")

	return placeholder
}

const (
	testPlaceholderID      = "placeholder"
	placeholderCreatedETag = `"p,1"`
)
