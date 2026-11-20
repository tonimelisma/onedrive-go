package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func databaseDownloadAction() *Action {
	return &Action{
		Type:    ActionDownload,
		Path:    "app.db",
		ItemID:  "item-db",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &PathView{
			Remote: &RemoteState{ItemID: "item-db", ETag: "etag-db"},
			Local:  &LocalState{Name: "app.db", ItemType: ItemTypeFile, Size: 3},
		},
	}
}
