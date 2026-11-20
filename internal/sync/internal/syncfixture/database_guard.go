package syncfixture

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func DatabaseDownloadAction() *syncdomain.Action {
	return &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "app.db",
		ItemID:  "item-db",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Remote: &syncdomain.RemoteState{ItemID: "item-db", ETag: "etag-db"},
			Local:  &syncdomain.LocalState{Name: "app.db", ItemType: syncdomain.ItemTypeFile, Size: 3},
		},
	}
}
