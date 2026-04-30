package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type testMockItemClient struct {
	createFolderFn      func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn          func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	moveItemIfMatchFn   func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	deleteItemFn        func(ctx context.Context, driveID driveid.ID, itemID string) error
	deleteItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	getItemFn           func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn     func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	listChildrenFn      func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

func (m *testMockItemClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return defaultMockGetItem(driveID, itemID), nil
}

func defaultMockGetItem(driveID driveid.ID, itemID string) *graph.Item {
	itemIDLower := strings.ToLower(itemID)
	isFolder := strings.Contains(itemIDLower, "parent") ||
		strings.Contains(itemIDLower, "folder") ||
		strings.Contains(itemIDLower, "root")

	return &graph.Item{
		ID:       itemID,
		DriveID:  driveID,
		IsFolder: isFolder,
	}
}

func (m *testMockItemClient) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	if m.getItemByPathFn != nil {
		return m.getItemByPathFn(ctx, driveID, remotePath)
	}

	return nil, graph.ErrNotFound
}

func (m *testMockItemClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *testMockItemClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return nil, fmt.Errorf("CreateFolder not mocked")
}

func (m *testMockItemClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemIfMatchFn != nil {
		return m.moveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, "")
	}
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItem not mocked")
}

func (m *testMockItemClient) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	ifMatch string,
) (*graph.Item, error) {
	if m.moveItemIfMatchFn != nil {
		return m.moveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, ifMatch)
	}
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItemIfMatch not mocked")
}

func (m *testMockItemClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemIfMatchFn != nil {
		return m.deleteItemIfMatchFn(ctx, driveID, itemID, "")
	}
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItem not mocked")
}

func (m *testMockItemClient) DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
	if m.deleteItemIfMatchFn != nil {
		return m.deleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
	}
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItemIfMatch not mocked")
}

func (m *testMockItemClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return fmt.Errorf("PermanentDeleteItem not mocked")
}
