package syncexec

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Every writer of a remote modification time stores the same value for the
// same time: the item converter, the upload outcome and the live row an
// executor precondition compares with the planned one. They are compared with
// each other, so one encoding at both ends of the range, and at the epoch.
//
// Validates: R-6.7.34
func TestStoredTimeNano_RemoteModificationTimeWritersAgree(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	for _, modified := range []time.Time{
		time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(0, 0),
		time.Date(2025, 6, 15, 12, 0, 0, 123456789, time.UTC),
	} {
		item := graph.Item{ID: "item-f", Name: "f.txt", ParentID: "root", DriveID: driveID, ModifiedAt: modified}
		converter := syncobserve.NewPrimaryConverter(syncfixture.EmptyBaseline(), driveID, synctest.TestLogger(t), nil, nil)
		events := converter.ConvertItems(context.Background(), []graph.Item{
			{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
			item,
		})
		var observed *syncdomain.RemoteItem
		for i := range events {
			if events[i].ItemID == item.ID {
				observed = &events[i]
			}
		}
		require.NotNil(t, observed, "%s", modified)

		uploaded := (&Executor{}).uploadOutcome(&syncdomain.Action{Type: syncdomain.ActionUpload, Path: "f.txt"}, driveID, "root",
			&driveops.UploadResult{Item: &item, Mtime: modified})
		live := remoteStateRowFromGraphItem(&item)

		want := syncdomain.StoredTimeNano(modified)
		assert.Equal(t, want, observed.Mtime, "converter, %s", modified)
		assert.Equal(t, want, uploaded.RemoteMtime, "upload outcome, %s", modified)
		assert.Equal(t, want, live.Mtime, "live precondition row, %s", modified)
	}
}
