package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Worker pool mock types (prefixed to avoid collision with executor_test.go)
// ---------------------------------------------------------------------------

type workerMockItemClient struct {
	createFolderFn func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn     func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn   func(ctx context.Context, driveID driveid.ID, itemID string) error
	getItemFn      func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

func (m *workerMockItemClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *workerMockItemClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *workerMockItemClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return nil, fmt.Errorf("CreateFolder not mocked")
}

func (m *workerMockItemClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return nil, fmt.Errorf("MoveItem not mocked")
}

func (m *workerMockItemClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return fmt.Errorf("DeleteItem not mocked")
}

type workerMockDownloader struct {
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *workerMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

type workerMockUploader struct {
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *workerMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newWorkerTestSetup(t *testing.T) (
	*ExecutorConfig, *BaselineManager, *Ledger, string,
) {
	t.Helper()

	mgr := newTestManager(t)
	ledger := NewLedger(mgr.DB(), testLogger(t))

	syncRoot := t.TempDir()
	driveID := driveid.New("0000000000000001")
	logger := testLogger(t)

	items := &workerMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			return &graph.Item{ID: "created-" + name, Name: name}, nil
		},
		deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
			return nil
		},
	}
	dl := &workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("file-content"))
			return int64(n), err
		},
	}
	ul := &workerMockUploader{}

	cfg := NewExecutorConfig(items, dl, ul, syncRoot, driveID, logger)
	cfg.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }
	cfg.sleepFunc = func(_ context.Context, _ time.Duration) error { return nil }

	return cfg, mgr, ledger, syncRoot
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestWorkerPool_FolderCreate(t *testing.T) {
	t.Parallel()

	cfg, mgr, ledger, syncRoot := newWorkerTestSetup(t)
	ctx := context.Background()

	actions := []Action{
		{
			Type:       ActionFolderCreate,
			Path:       "Documents",
			DriveID:    driveid.New("0000000000000001"),
			ItemID:     "folder-doc",
			CreateSide: CreateLocal,
			View: &PathView{
				Remote: &RemoteState{
					ItemID:   "folder-doc",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "root",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	ids, writeErr := ledger.WriteActions(ctx, actions, nil, "cycle-wp1")
	if writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	tracker := NewDepTracker(10, 10, testLogger(t))
	tracker.Add(&actions[0], ids[0], nil)

	pool := NewWorkerPool(cfg, tracker, mgr, ledger, testLogger(t))
	pool.Start(ctx, 4)
	pool.Wait()
	pool.Stop()

	succeeded, failed, errs := pool.Stats()
	if failed != 0 {
		t.Errorf("failed = %d, want 0; errors: %v", failed, errs)
	}

	if succeeded != 1 {
		t.Errorf("succeeded = %d, want 1", succeeded)
	}

	// Verify directory was created.
	info, statErr := os.Stat(filepath.Join(syncRoot, "Documents"))
	if statErr != nil {
		t.Fatalf("stat Documents: %v", statErr)
	}

	if !info.IsDir() {
		t.Error("Documents should be a directory")
	}

	// Verify baseline was updated.
	bl, loadErr := mgr.Load(ctx)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}

	if _, ok := bl.GetByPath("Documents"); !ok {
		t.Error("baseline entry not found for Documents")
	}

	// Verify ledger action is done.
	pending, countErr := ledger.CountPendingForCycle(ctx, "cycle-wp1")
	if countErr != nil {
		t.Fatalf("CountPending: %v", countErr)
	}

	if pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
}

func TestWorkerPool_DependencyChain(t *testing.T) {
	t.Parallel()

	cfg, mgr, ledger, syncRoot := newWorkerTestSetup(t)
	ctx := context.Background()

	// Folder create → then download into that folder.
	actions := []Action{
		{
			Type:       ActionFolderCreate,
			Path:       "NewDir",
			DriveID:    driveid.New("0000000000000001"),
			CreateSide: CreateLocal,
			View: &PathView{
				Remote: &RemoteState{
					ItemID:   "newdir-id",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "root",
					ItemType: ItemTypeFolder,
				},
			},
		},
		{
			Type:    ActionDownload,
			Path:    "NewDir/file.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "file-id",
			View: &PathView{
				Remote: &RemoteState{
					ItemID:   "file-id",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "newdir-id",
					Size:     12,
					Hash:     "testhash",
				},
			},
		},
	}

	deps := [][]int{{}, {0}} // action 1 depends on action 0
	ids, writeErr := ledger.WriteActions(ctx, actions, deps, "cycle-wp2")
	if writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	tracker := NewDepTracker(10, 10, testLogger(t))
	tracker.Add(&actions[0], ids[0], nil)
	tracker.Add(&actions[1], ids[1], []int64{ids[0]})

	pool := NewWorkerPool(cfg, tracker, mgr, ledger, testLogger(t))
	pool.Start(ctx, 4)
	pool.Wait()
	pool.Stop()

	succeeded, failed, errs := pool.Stats()
	if failed != 0 {
		t.Errorf("failed = %d, want 0; errors: %v", failed, errs)
	}

	if succeeded != 2 {
		t.Errorf("succeeded = %d, want 2", succeeded)
	}

	// Verify file was downloaded.
	content, readErr := os.ReadFile(filepath.Join(syncRoot, "NewDir/file.txt"))
	if readErr != nil {
		t.Fatalf("read file: %v", readErr)
	}

	if string(content) != "file-content" {
		t.Errorf("file content = %q, want %q", content, "file-content")
	}
}

func TestWorkerPool_StopCancelsWork(t *testing.T) {
	t.Parallel()

	cfg, mgr, ledger, _ := newWorkerTestSetup(t)
	ctx := context.Background()

	actions := []Action{
		{
			Type:    ActionDownload,
			Path:    "slow.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "slow-id",
			View: &PathView{
				Remote: &RemoteState{
					ItemID:  "slow-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    100,
				},
			},
		},
	}

	ids, writeErr := ledger.WriteActions(ctx, actions, nil, "cycle-wp3")
	if writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	tracker := NewDepTracker(10, 10, testLogger(t))
	tracker.Add(&actions[0], ids[0], nil)

	pool := NewWorkerPool(cfg, tracker, mgr, ledger, testLogger(t))
	pool.Start(ctx, 4)

	// Give workers a moment to pick up the action.
	time.Sleep(50 * time.Millisecond)

	// Stop should not hang.
	done := make(chan struct{})
	go func() {
		pool.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within timeout")
	}
}

func TestWorkerPool_Stats(t *testing.T) {
	t.Parallel()

	cfg, mgr, ledger, _ := newWorkerTestSetup(t)
	ctx := context.Background()

	// Use a delete action against a nonexistent local file — the delete should
	// still succeed (deleteLocalFile succeeds when file doesn't exist).
	actions := []Action{
		{
			Type:    ActionLocalDelete,
			Path:    "nonexistent.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "del-id",
			View:    &PathView{},
		},
	}

	ids, writeErr := ledger.WriteActions(ctx, actions, nil, "cycle-wp4")
	if writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	tracker := NewDepTracker(10, 10, testLogger(t))
	tracker.Add(&actions[0], ids[0], nil)

	pool := NewWorkerPool(cfg, tracker, mgr, ledger, testLogger(t))
	pool.Start(ctx, 4)
	pool.Wait()
	pool.Stop()

	succeeded, _, _ := pool.Stats()
	if succeeded < 1 {
		t.Errorf("succeeded = %d, want >= 1", succeeded)
	}
}

// TestWorkerPool_FailedOutcome_MarksLedgerFailed verifies that when an action
// execution fails, the worker marks the ledger row as "failed" (not "claimed").
// Regression test for: worker never called failAndComplete for execution failures.
func TestWorkerPool_FailedOutcome_MarksLedgerFailed(t *testing.T) {
	t.Parallel()

	cfg, mgr, ledger, _ := newWorkerTestSetup(t)
	ctx := context.Background()

	// Configure a download mock that always fails.
	cfg.downloads = &workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, fmt.Errorf("simulated download failure")
		},
	}

	actions := []Action{
		{
			Type:    ActionDownload,
			Path:    "fail-me.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "fail-id",
			View: &PathView{
				Remote: &RemoteState{
					ItemID:  "fail-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    10,
					Hash:    "somehash",
				},
			},
		},
	}

	ids, writeErr := ledger.WriteActions(ctx, actions, nil, "cycle-fail-ledger")
	if writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	tracker := NewDepTracker(10, 10, testLogger(t))
	tracker.Add(&actions[0], ids[0], nil)

	pool := NewWorkerPool(cfg, tracker, mgr, ledger, testLogger(t))
	pool.Start(ctx, 4)
	pool.Wait()
	pool.Stop()

	succeeded, failed, errs := pool.Stats()
	if succeeded != 0 {
		t.Errorf("succeeded = %d, want 0", succeeded)
	}

	if failed < 1 {
		t.Errorf("failed = %d, want >= 1; errors: %v", failed, errs)
	}

	// The ledger row should NOT be pending (it was claimed+failed, not stuck as claimed).
	pending, countErr := ledger.CountPendingForCycle(ctx, "cycle-fail-ledger")
	if countErr != nil {
		t.Fatalf("CountPending: %v", countErr)
	}

	if pending != 0 {
		t.Errorf("pending = %d, want 0 (failed action should be marked as failed, not stuck as claimed)", pending)
	}
}
