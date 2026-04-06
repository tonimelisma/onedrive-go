package sync

// This file provides executor mock types and test helper functions for
// fault_injection_test.go. The helpers were previously defined in
// executor_test.go but that file was migrated to internal/syncexec.
// Since fault_injection_test.go tests the sync package's syncexec.WorkerPool
// integration (using the type aliases in types.go), these helpers must
// live in the same test package.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// executorMockItemClient is a test double for synctypes.ItemClient.
type executorMockItemClient struct {
	createFolderFn  func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn      func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn    func(ctx context.Context, driveID driveid.ID, itemID string) error
	getItemFn       func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	listChildrenFn  func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

func (m *executorMockItemClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *executorMockItemClient) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	if m.getItemByPathFn != nil {
		return m.getItemByPathFn(ctx, driveID, remotePath)
	}

	return nil, graph.ErrNotFound
}

func (m *executorMockItemClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *executorMockItemClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return nil, fmt.Errorf("CreateFolder not mocked")
}

func (m *executorMockItemClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItem not mocked")
}

func (m *executorMockItemClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItem not mocked")
}

func (m *executorMockItemClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return fmt.Errorf("PermanentDeleteItem not mocked")
}

// executorMockDownloader is a test double for driveops.Downloader.
type executorMockDownloader struct {
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *executorMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

// executorMockUploader is a test double for driveops.Uploader.
type executorMockUploader struct {
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *executorMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

// newTestExecutorConfig creates a test syncexec.ExecutorConfig with a temp sync root,
// injecting the provided mock item client, downloader, and uploader.
func newTestExecutorConfig(t *testing.T, items *executorMockItemClient, dl *executorMockDownloader, ul *executorMockUploader) (*syncexec.ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(testDriveID)
	logger := testLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := syncexec.NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, nil)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })

	return cfg, syncRoot
}

// writeExecTestFile creates a file at dir/relPath with the given content,
// creating parent directories as needed. Returns the absolute path.
func writeExecTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	absPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o700))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600))

	return absPath
}
