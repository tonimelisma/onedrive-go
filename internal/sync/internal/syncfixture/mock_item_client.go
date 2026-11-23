package syncfixture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
	// GetChildByNameFn and ListChildrenPagesFn, when set, replace the reads
	// derived from ListChildrenFn, for a test that serves pages or counts
	// requests.
	GetChildByNameFn    func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	ListChildrenPagesFn func(ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc) error
}

// TestMockItemClient is pinned to the sync item client it stands in for.
var _ syncdomain.ItemClient = (*TestMockItemClient)(nil)

// GetChildByName answers a read of one name as Graph answers it, from the
// folder ListChildrenFn holds (ChildNamed).
func (m *TestMockItemClient) GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.GetChildByNameFn != nil {
		return m.GetChildByNameFn(ctx, driveID, parentID, name)
	}
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, err
	}

	return ChildNamed(children, name)
}

// ListChildrenPages hands the folder ListChildrenFn holds over as one page.
func (m *TestMockItemClient) ListChildrenPages(
	ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc,
) error {
	if m.ListChildrenPagesFn != nil {
		return m.ListChildrenPagesFn(ctx, driveID, parentID, visit)
	}
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return err
	}
	visit(children)

	return nil
}

// ChildNamed is what Graph's read of one name answers from a folder holding
// children: the child that holds the name under OneDrive's name equivalence,
// the one spelled as asked first (namekey.Find; OneDrive holds one item per
// name, so a real folder never makes the choice), else graph.ErrNotFound. A
// folder of children that hold the name in more than one spelling, none as
// asked, answers ErrNotFound too, so the caller's listing decides.
func ChildNamed(children []graph.Item, name string) (*graph.Item, error) {
	match := namekey.Find(children, name, func(child *graph.Item) (string, bool) { return child.Name, true })
	if !match.Found() {
		return nil, fmt.Errorf("reading %q: %w", name, graph.ErrNotFound)
	}
	child := children[match.Index]

	return &child, nil
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
