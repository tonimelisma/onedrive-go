package sync

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// emptyQuickXor is the hash OneDrive gives an empty file (L-17, L-01).
const emptyQuickXor = "AAAAAAAAAAAAAAAAAAAAAAAAAAA="

// creatingTestUploader models graph.Client's two-step create for executor
// tests: create runs where the empty item is made, with the record hook the
// client calls between the item and its content. getItem answers the transfer
// manager's lookups of an upload's item (nil fails them), and every removal
// of an empty item the transfer manager asks for works and is recorded in
// removedEmpty.
type creatingTestUploader struct {
	executorMockUploader
	create       func(ctx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error)
	getItem      func(itemID string) (*graph.Item, error)
	removedEmpty []string
}

func (u *creatingTestUploader) UploadCreating(
	ctx context.Context, _ driveid.ID, _, _ string,
	_ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	return u.create(ctx, onCreated)
}

func (u *creatingTestUploader) GetItem(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
	if u.getItem == nil {
		return nil, fmt.Errorf("GetItem of %s not mocked", itemID)
	}

	return u.getItem(itemID)
}

// GetChildByName and ListChildrenPages make the uploader the transfer
// manager's driveops.ItemReader; both refuse, as a create's outcome is looked
// up by its item ID.
func (u *creatingTestUploader) GetChildByName(context.Context, driveid.ID, string, string) (*graph.Item, error) {
	return nil, fmt.Errorf("a create's outcome is looked up by its item ID, never by its name")
}

func (u *creatingTestUploader) ListChildrenPages(context.Context, driveid.ID, string, graph.ChildrenPageFunc) error {
	return fmt.Errorf("a create's outcome is looked up by its item ID, never by listing its folder")
}

func (u *creatingTestUploader) RemoveEmptyItem(_ context.Context, _ driveid.ID, itemID string) (bool, error) {
	u.removedEmpty = append(u.removedEmpty, itemID)
	return true, nil
}
