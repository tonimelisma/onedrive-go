package sync

import (
	"context"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func remoteFolderDeleteAction() *Action {
	const (
		path   = "X"
		itemID = "folder-x"
	)
	driveID := driveid.New(synctest.TestDriveID)
	return &Action{
		Type:    ActionRemoteDelete,
		Path:    path,
		ItemID:  itemID,
		DriveID: driveID,
		View: &PathView{
			Path:     path,
			Baseline: &BaselineEntry{Path: path, ItemID: itemID, ItemType: ItemTypeFolder},
			Remote:   &RemoteState{DriveID: driveID, ItemID: itemID, ItemType: ItemTypeFolder, ETag: "etag-folder"},
		},
	}
}

func folderDeleteItems(t *testing.T, children map[string][]graph.Item, deleted *[]string) *executorMockItemClient {
	t.Helper()

	driveID := driveid.New(synctest.TestDriveID)
	return &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: "X", ParentID: "root", IsFolder: true, DriveID: driveID, ETag: "etag-live"}, nil
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
			return children[parentID], nil
		},
		deleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
	}
}
