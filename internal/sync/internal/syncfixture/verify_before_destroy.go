package syncfixture

import (
	"context"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func RemoteFolderDeleteAction() *syncdomain.Action {
	const (
		path   = "X"
		itemID = "folder-x"
	)
	driveID := driveid.New(synctest.TestDriveID)
	return &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    path,
		ItemID:  itemID,
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path:     path,
			Baseline: &syncdomain.BaselineEntry{Path: path, ItemID: itemID, ItemType: syncdomain.ItemTypeFolder},
			Remote:   &syncdomain.RemoteState{DriveID: driveID, ItemID: itemID, ItemType: syncdomain.ItemTypeFolder, ETag: "etag-folder"},
		},
	}
}

func FolderDeleteItems(t *testing.T, children map[string][]graph.Item, deleted *[]string) *ExecutorMockItemClient {
	t.Helper()

	driveID := driveid.New(synctest.TestDriveID)
	return &ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: "X", ParentID: "root", IsFolder: true, DriveID: driveID, ETag: "etag-live"}, nil
		},
		ListChildrenFn: func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
			return children[parentID], nil
		},
		DeleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
		DeleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
	}
}
