package syncfixture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type TestMockItemClient struct {
	CreateFolderFn      func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItemFn          func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	MoveItemIfMatchFn   func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	DeleteItemFn        func(ctx context.Context, driveID driveid.ID, itemID string) error
	DeleteItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	GetItemFn           func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn     func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	ListChildrenFn      func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

func (m *TestMockItemClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.GetItemFn != nil {
		return m.GetItemFn(ctx, driveID, itemID)
	}

	return DefaultMockGetItem(driveID, itemID), nil
}

func DefaultMockGetItem(driveID driveid.ID, itemID string) *graph.Item {
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

func (m *TestMockItemClient) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	if m.getItemByPathFn != nil {
		return m.getItemByPathFn(ctx, driveID, remotePath)
	}

	return nil, graph.ErrNotFound
}

func (m *TestMockItemClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.ListChildrenFn != nil {
		return m.ListChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *TestMockItemClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.CreateFolderFn != nil {
		return m.CreateFolderFn(ctx, driveID, parentID, name)
	}

	return nil, fmt.Errorf("CreateFolder not mocked")
}

func (m *TestMockItemClient) MoveItem(
	ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, _ time.Time,
) (*graph.Item, error) {
	if m.MoveItemIfMatchFn != nil {
		return m.MoveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, "")
	}
	if m.MoveItemFn != nil {
		return m.MoveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItem not mocked")
}

func (m *TestMockItemClient) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	opts graph.MoveOptions,
) (*graph.Item, error) {
	if m.MoveItemIfMatchFn != nil {
		return m.MoveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, opts.IfMatch)
	}
	if m.MoveItemFn != nil {
		return m.MoveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItemIfMatch not mocked")
}

func (m *TestMockItemClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.DeleteItemIfMatchFn != nil {
		return m.DeleteItemIfMatchFn(ctx, driveID, itemID, "")
	}
	if m.DeleteItemFn != nil {
		return m.DeleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItem not mocked")
}

func (m *TestMockItemClient) DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
	if m.DeleteItemIfMatchFn != nil {
		return m.DeleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
	}
	if m.DeleteItemFn != nil {
		return m.DeleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItemIfMatch not mocked")
}

func (m *TestMockItemClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return fmt.Errorf("PermanentDeleteItem not mocked")
}
