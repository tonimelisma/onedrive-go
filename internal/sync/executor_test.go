package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Mock types (prefixed to avoid collision with other test files)
// ---------------------------------------------------------------------------

type executorMockItemClient struct {
	createFolderFn func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn     func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn   func(ctx context.Context, driveID driveid.ID, itemID string) error
	getItemFn      func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

func (m *executorMockItemClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
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

type executorMockDownloader struct {
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *executorMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

type executorMockUploader struct {
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *executorMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestExecutorConfig(t *testing.T, items *executorMockItemClient, dl *executorMockDownloader, ul *executorMockUploader) (*ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(testDriveID)
	logger := testLogger(t)

	cfg := NewExecutorConfig(items, dl, ul, syncRoot, driveID, logger)
	cfg.transferMgr = driveops.NewTransferManager(dl, ul, nil, logger)
	cfg.nowFunc = func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) }
	cfg.sleepFunc = func(_ context.Context, _ time.Duration) error { return nil }

	return cfg, syncRoot
}

func writeExecTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	absPath := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return absPath
}

func requireOutcomeSuccess(t *testing.T, o Outcome) {
	t.Helper()

	if !o.Success {
		t.Fatalf("expected success but got error: %v", o.Error)
	}
}

func requireOutcomeFailure(t *testing.T, o Outcome) {
	t.Helper()

	if o.Success {
		t.Fatal("expected failure but got success")
	}
}

// ---------------------------------------------------------------------------
// Folder create tests
// ---------------------------------------------------------------------------

func TestExecutor_CreateLocalFolder(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "docs/notes",
		CreateSide: CreateLocal,
		View: &PathView{
			Remote: &RemoteState{
				ItemID:   "folder1",
				ParentID: "root",
				Mtime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.executeFolderCreate(context.Background(), action)
	requireOutcomeSuccess(t, o)

	info, err := os.Stat(filepath.Join(syncRoot, "docs/notes"))
	if err != nil {
		t.Fatalf("folder not created: %v", err)
	}

	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestExecutor_CreateRemoteFolder(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			if parentID != "root" || name != "photos" {
				t.Errorf("unexpected args: parentID=%s name=%s", parentID, name)
			}

			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		View:       &PathView{Path: "photos"},
	}

	o := e.executeFolderCreate(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.ItemID != "new-folder-id" {
		t.Errorf("expected item_id=new-folder-id, got %s", o.ItemID)
	}
}

func TestExecutor_CreateRemoteFolder_Error(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "restricted",
		CreateSide: CreateRemote,
		View:       &PathView{Path: "restricted"},
	}

	o := e.executeFolderCreate(context.Background(), action)
	requireOutcomeFailure(t, o)
}

// ---------------------------------------------------------------------------
// Move tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "old-name.txt", "content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View:    &PathView{Path: "new-name.txt"},
	}

	o := e.executeMove(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if _, err := os.Stat(filepath.Join(syncRoot, "old-name.txt")); !os.IsNotExist(err) {
		t.Error("old path still exists")
	}

	if _, err := os.Stat(filepath.Join(syncRoot, "new-name.txt")); err != nil {
		t.Error("new path not created")
	}
}

func TestExecutor_LocalMove_SourceMissing(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "target.txt",
		OldPath: "nonexistent.txt",
		View:    &PathView{Path: "target.txt"},
	}

	o := e.executeMove(context.Background(), action)
	requireOutcomeFailure(t, o)
}

func TestExecutor_RemoteMove(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			if itemID != "item1" || newParentID != "root" || newName != "renamed.txt" {
				t.Errorf("unexpected move args: itemID=%s parentID=%s name=%s", itemID, newParentID, newName)
			}

			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Path: "renamed.txt"},
	}

	o := e.executeMove(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.Path != "renamed.txt" {
		t.Errorf("expected path=renamed.txt, got %s", o.Path)
	}

	if o.OldPath != "original.txt" {
		t.Errorf("expected old_path=original.txt, got %s", o.OldPath)
	}
}

// ---------------------------------------------------------------------------
// Download tests
// ---------------------------------------------------------------------------

func TestExecutor_Download_Success(t *testing.T) {
	t.Parallel()

	execFileContent := "hello world"

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte(execFileContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "greetings.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Remote: &RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				ETag:     "etag1",
				Mtime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	data, err := os.ReadFile(filepath.Join(syncRoot, "greetings.txt"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}

	if string(data) != execFileContent {
		t.Errorf("expected %q, got %q", execFileContent, string(data))
	}

	// Partial file should not exist.
	if _, err := os.Stat(filepath.Join(syncRoot, "greetings.txt.partial")); !os.IsNotExist(err) {
		t.Error(".partial file still exists")
	}

	if o.Size != int64(len(execFileContent)) {
		t.Errorf("expected size=%d, got %d", len(execFileContent), o.Size)
	}
}

func TestExecutor_Download_APIError(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "exec-forbidden.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeFailure(t, o)
}

func TestExecutor_Download_ParentDirCreated(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "deep/nested/dir/exec-dl.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{Mtime: 1}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if _, err := os.Stat(filepath.Join(syncRoot, "deep/nested/dir/exec-dl.txt")); err != nil {
		t.Fatal("file not created in nested dir")
	}
}

func TestExecutor_Download_ZeroByte(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "exec-empty.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	info, err := os.Stat(filepath.Join(syncRoot, "exec-empty.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Size() != 0 {
		t.Errorf("expected zero-byte file, got %d bytes", info.Size())
	}
}

// ---------------------------------------------------------------------------
// Download hash mismatch tests (B-132)
// ---------------------------------------------------------------------------

func TestExecutor_Download_HashMismatch_Retries(t *testing.T) {
	t.Parallel()

	callCount := 0

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			// First two attempts return wrong content, third returns correct.
			if callCount < 3 {
				n, err := w.Write([]byte("wrong content"))
				return int64(n), err
			}

			n, err := w.Write([]byte("hello world"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	correctHash := "aCgDG9jwBhDc4Q1yawMZAAAAAAA=" // QuickXorHash of "hello world"
	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-retry.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if callCount != 3 {
		t.Errorf("expected 3 download calls, got %d", callCount)
	}

	if o.LocalHash != correctHash {
		t.Errorf("LocalHash = %q, want %q", o.LocalHash, correctHash)
	}

	if o.RemoteHash != correctHash {
		t.Errorf("RemoteHash = %q, want %q", o.RemoteHash, correctHash)
	}

	// File should contain correct content.
	data, err := os.ReadFile(filepath.Join(syncRoot, "hash-retry.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "hello world" {
		t.Errorf("file content = %q, want %q", string(data), "hello world")
	}
}

func TestExecutor_Download_HashMismatch_Accepted(t *testing.T) {
	t.Parallel()

	callCount := 0

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			// Always return wrong content to exhaust retries.
			n, err := w.Write([]byte("wrong content"))
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-accept.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{Hash: "stale-remote-hash", Mtime: 1}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	// All retries exhausted: 1 initial + 2 retries = 3.
	if callCount != 3 {
		t.Errorf("expected 3 download calls, got %d", callCount)
	}

	// After exhaustion, remoteHash is overridden to localHash to prevent baseline mismatch loop.
	if o.LocalHash != o.RemoteHash {
		t.Errorf("expected LocalHash == RemoteHash after exhaustion, got local=%q remote=%q",
			o.LocalHash, o.RemoteHash)
	}
}

func TestExecutor_Download_HashMatch_NoRetry(t *testing.T) {
	t.Parallel()

	callCount := 0
	content := "hello world"

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			n, err := w.Write([]byte(content))
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	correctHash := "aCgDG9jwBhDc4Q1yawMZAAAAAAA="
	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-ok.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if callCount != 1 {
		t.Errorf("expected 1 download call, got %d", callCount)
	}

	if o.LocalHash != correctHash {
		t.Errorf("LocalHash = %q, want %q", o.LocalHash, correctHash)
	}
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

func TestExecutor_Upload_SimpleSuccess(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			if parentID != "root" || name != "exec-small.txt" {
				t.Errorf("unexpected args: parentID=%s name=%s", parentID, name)
			}

			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Path: "exec-small.txt"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.ItemID != "uploaded1" {
		t.Errorf("expected item_id=uploaded1, got %s", o.ItemID)
	}

	if o.ParentID != "root" {
		t.Errorf("expected parent_id=root, got %s", o.ParentID)
	}
}

func TestExecutor_Upload_ParentFromBaseline(t *testing.T) {
	t.Parallel()

	var capturedParentID string

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedParentID = parentID
			return &graph.Item{ID: "uploaded3", ETag: "e3"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "exec-existing-dir",
		ItemID:   "baseline-folder-id",
		DriveID:  driveid.New(testDriveID),
		ItemType: ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "exec-existing-dir/exec-doc.txt", "content")

	action := &Action{
		Type: ActionUpload,
		Path: "exec-existing-dir/exec-doc.txt",
		View: &PathView{Path: "exec-existing-dir/exec-doc.txt"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if capturedParentID != "baseline-folder-id" {
		t.Errorf("expected parent from baseline, got %s", capturedParentID)
	}
}

func TestExecutor_Upload_B068_ZeroDriveIDFilled(t *testing.T) {
	t.Parallel()

	var capturedDriveID driveid.ID

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, driveID driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			return &graph.Item{ID: "up1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-new-file.txt", "data")

	// New local item has zero DriveID (EF13 scenario).
	action := &Action{
		Type: ActionUpload,
		Path: "exec-new-file.txt",
		View: &PathView{Path: "exec-new-file.txt"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	// Executor should have filled driveID from its own context.
	if capturedDriveID != driveid.New(testDriveID) {
		t.Errorf("expected executor driveID, got %s", capturedDriveID)
	}
}

func TestExecutor_Upload_LargeFileSuccess(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "chunked1", ETag: "ce1", QuickXorHash: "hash1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	// Create a file > 4 MiB to exercise Upload for large files.
	bigContent := strings.Repeat("x", 5*1024*1024) // 5 MiB
	writeExecTestFile(t, syncRoot, "exec-big-file.bin", bigContent)

	action := &Action{
		Type: ActionUpload,
		Path: "exec-big-file.bin",
		View: &PathView{Path: "exec-big-file.bin"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.ItemID != "chunked1" {
		t.Errorf("expected item_id=chunked1, got %s", o.ItemID)
	}
}

// ---------------------------------------------------------------------------
// Local delete tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalDelete_HashMatch(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "exec-delete-me.txt", "content")

	hash, err := driveops.ComputeQuickXorHash(absPath)
	if err != nil {
		t.Fatal(err)
	}

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-delete-me.txt",
		ItemID: "item1",
		View: &PathView{
			Baseline: &BaselineEntry{LocalHash: hash},
		},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestExecutor_LocalDelete_HashMismatch_ConflictCopy(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-modified.txt", "new content")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &PathView{
			Baseline: &BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	// B-133: outcome should be ActionConflict (not ActionLocalDelete) so it's tracked.
	if o.Action != ActionConflict {
		t.Errorf("expected ActionConflict, got %s", o.Action)
	}

	if o.ConflictType != ConflictEditDelete {
		t.Errorf("expected ConflictEditDelete, got %q", o.ConflictType)
	}

	if o.RemoteHash != "baseline-remote-hash" {
		t.Errorf("RemoteHash = %q, want %q", o.RemoteHash, "baseline-remote-hash")
	}

	// Original should be gone.
	if _, err := os.Stat(filepath.Join(syncRoot, "exec-modified.txt")); !os.IsNotExist(err) {
		t.Error("original file should have been renamed")
	}

	// Conflict copy should exist.
	entries, _ := os.ReadDir(syncRoot)
	found := false

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			found = true
		}
	}

	if !found {
		t.Error("expected conflict copy to be created")
	}
}

func TestExecutor_LocalDelete_AlreadyGone(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-already-gone.txt",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)
}

func TestExecutor_LocalDelete_FolderEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	if err := os.MkdirAll(filepath.Join(syncRoot, "exec-empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-empty-dir",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if _, err := os.Stat(filepath.Join(syncRoot, "exec-empty-dir")); !os.IsNotExist(err) {
		t.Error("directory should have been removed")
	}
}

func TestExecutor_LocalDelete_FolderNotEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-non-empty-dir/child.txt", "data")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-non-empty-dir",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeFailure(t, o)
}

// ---------------------------------------------------------------------------
// Remote delete tests
// ---------------------------------------------------------------------------

func TestExecutor_RemoteDelete_Success(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			if itemID != "item1" {
				t.Errorf("unexpected itemID: %s", itemID)
			}

			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{},
	}

	o := e.executeRemoteDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)
}

func TestExecutor_RemoteDelete_404IsSuccess(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
			return graph.ErrNotFound
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-already-deleted.txt",
		ItemID:  "item2",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{},
	}

	o := e.executeRemoteDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)
}

func TestExecutor_RemoteDelete_403Skip(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
			return graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-forbidden-del.txt",
		ItemID:  "item3",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{},
	}

	o := e.executeRemoteDelete(context.Background(), action)
	requireOutcomeFailure(t, o)
}

// ---------------------------------------------------------------------------
// Conflict tests
// ---------------------------------------------------------------------------

func TestExecutor_Conflict_EditEdit_KeepBoth(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")

	action := &Action{
		Type:    ActionConflict,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Remote: &RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				ETag:     "etag1",
			},
		},
		ConflictInfo: &ConflictRecord{ConflictType: "edit_edit"},
	}

	o := e.executeConflict(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.ConflictType != "edit_edit" {
		t.Errorf("expected conflict_type=edit_edit, got %s", o.ConflictType)
	}

	// Original path should have remote content.
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-conflict.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "remote version" {
		t.Errorf("expected remote content, got %q", string(data))
	}

	// Conflict copy should have local content.
	entries, _ := os.ReadDir(syncRoot)
	conflictFound := false

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			conflictData, _ := os.ReadFile(filepath.Join(syncRoot, entry.Name()))
			if string(conflictData) == "local version" {
				conflictFound = true
			}
		}
	}

	if !conflictFound {
		t.Error("expected conflict copy with local content")
	}
}

func TestExecutor_Conflict_EditDelete_AutoResolve(t *testing.T) {
	t.Parallel()

	var uploadCalled bool

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:   "new-item",
				Name: name,
				ETag: "etag-new",
			}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	// Local file exists with modified content (edit-delete: local modified,
	// remote deleted).
	writeExecTestFile(t, syncRoot, "exec-ed-file.txt", "locally modified data")

	action := &Action{
		Type:    ActionConflict,
		Path:    "exec-ed-file.txt",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Path: "exec-ed-file.txt"},
		ConflictInfo: &ConflictRecord{
			ConflictType: "edit_delete",
			DriveID:      driveid.New(testDriveID),
		},
	}

	o := e.executeConflict(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if !uploadCalled {
		t.Error("expected upload to be called for edit-delete auto-resolve")
	}

	if o.Action != ActionConflict {
		t.Errorf("expected action=conflict, got %s", o.Action)
	}

	if o.ConflictType != "edit_delete" {
		t.Errorf("expected conflict_type=edit_delete, got %s", o.ConflictType)
	}

	if o.ResolvedBy != "auto" {
		t.Errorf("expected resolved_by=auto, got %q", o.ResolvedBy)
	}

	if o.ItemID != "new-item" {
		t.Errorf("expected item_id=new-item, got %s", o.ItemID)
	}

	// Local file should still exist with original content (not modified by upload).
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-ed-file.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "locally modified data" {
		t.Errorf("expected local content preserved, got %q", string(data))
	}
}

// ---------------------------------------------------------------------------
// Conflict copy naming tests
// ---------------------------------------------------------------------------

func TestConflictCopyPath_Normal(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/exec-file.txt", ts)
	expected := "/sync/root/exec-file.conflict-20260115-123045.txt"

	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestConflictCopyPath_Dotfile(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/.bashrc", ts)
	expected := "/sync/root/.bashrc.conflict-20260115-123045"

	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestConflictCopyPath_MultiDot(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/archive.tar.gz", ts)
	expected := "/sync/root/archive.tar.conflict-20260115-123045.gz"

	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

// ---------------------------------------------------------------------------
// Synced update tests
// ---------------------------------------------------------------------------

func TestExecutor_SyncedUpdate(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionUpdateSynced,
		Path:    "exec-converged.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Remote: &RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				Hash:     "hash1",
				Size:     1024,
				ETag:     "etag1",
				ItemType: ItemTypeFile,
			},
			Local: &LocalState{
				Hash:  "hash1",
				Mtime: 1234567890,
			},
		},
	}

	o := e.executeSyncedUpdate(action)
	requireOutcomeSuccess(t, o)

	if o.RemoteHash != "hash1" {
		t.Errorf("expected remote_hash=hash1, got %s", o.RemoteHash)
	}

	if o.LocalHash != "hash1" {
		t.Errorf("expected local_hash=hash1, got %s", o.LocalHash)
	}

	if o.Size != 1024 {
		t.Errorf("expected size=1024, got %d", o.Size)
	}

	if o.Mtime != 1234567890 {
		t.Errorf("expected mtime=1234567890, got %d", o.Mtime)
	}
}

// ---------------------------------------------------------------------------
// Cleanup tests
// ---------------------------------------------------------------------------

func TestExecutor_Cleanup(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionCleanup,
		Path:    "exec-ghost.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
	}

	o := e.executeCleanup(action)
	requireOutcomeSuccess(t, o)

	if o.Action != ActionCleanup {
		t.Errorf("expected action=cleanup, got %s", o.Action)
	}

	if o.Path != "exec-ghost.txt" {
		t.Errorf("expected path=exec-ghost.txt, got %s", o.Path)
	}
}

// ---------------------------------------------------------------------------
// Error classification tests
// ---------------------------------------------------------------------------

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected errClass
	}{
		{"nil", nil, errClassSkip},
		{"context canceled", context.Canceled, errClassFatal},
		{"context deadline", context.DeadlineExceeded, errClassFatal},
		{"unauthorized", graph.ErrUnauthorized, errClassFatal},
		{"throttled", graph.ErrThrottled, errClassRetryable},
		{"server error", graph.ErrServerError, errClassRetryable},
		{"not found", graph.ErrNotFound, errClassSkip},
		{"forbidden", graph.ErrForbidden, errClassSkip},
		{"locked", graph.ErrLocked, errClassSkip},
		{"bad request", graph.ErrBadRequest, errClassSkip},
		{"generic error", errors.New("something broke"), errClassSkip},
		{"wrapped unauthorized", fmt.Errorf("auth: %w", graph.ErrUnauthorized), errClassFatal},
		{"507 insufficient storage", &graph.GraphError{StatusCode: 507, Err: graph.ErrServerError}, errClassFatal},
		{"408 request timeout", &graph.GraphError{StatusCode: 408}, errClassRetryable},
		{"412 precondition failed", &graph.GraphError{StatusCode: 412}, errClassRetryable},
		{"509 bandwidth exceeded", &graph.GraphError{StatusCode: 509}, errClassRetryable},
		{"401 via GraphError", &graph.GraphError{StatusCode: 401, Err: graph.ErrUnauthorized}, errClassFatal},
		{"429 via GraphError", &graph.GraphError{StatusCode: 429, Err: graph.ErrThrottled}, errClassRetryable},
		{"500 via GraphError", &graph.GraphError{StatusCode: 500, Err: graph.ErrServerError}, errClassRetryable},
		{"502 via GraphError", &graph.GraphError{StatusCode: 502, Err: graph.ErrServerError}, errClassRetryable},
		{"403 via GraphError", &graph.GraphError{StatusCode: 403, Err: graph.ErrForbidden}, errClassSkip},
		{"423 locked via GraphError", &graph.GraphError{StatusCode: 423, Err: graph.ErrLocked}, errClassRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyError(tt.err)
			if got != tt.expected {
				t.Errorf("classifyError(%v) = %d, want %d", tt.err, got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Parent ID resolution tests
// ---------------------------------------------------------------------------

func TestExecutor_ResolveParentID_Baseline(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "exec-existing-folder",
		ItemID:   "folder-id-from-baseline",
		DriveID:  driveid.New(testDriveID),
		ItemType: ItemTypeFolder,
	}))

	tests := []struct {
		name       string
		relPath    string
		expectedID string
		expectErr  bool
	}{
		{"root level", "exec-file.txt", "root", false},
		{"from baseline", "exec-existing-folder/child.txt", "folder-id-from-baseline", false},
		{"unknown parent", "exec-unknown/child.txt", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id, err := e.resolveParentID(tt.relPath)
			if tt.expectErr {
				if err == nil {
					t.Error("expected error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if id != tt.expectedID {
				t.Errorf("expected %s, got %s", tt.expectedID, id)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// StemExt helper tests
// ---------------------------------------------------------------------------

func TestConflictStemExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantStem string
		wantExt  string
	}{
		{"normal", "exec-file.txt", "exec-file", ".txt"},
		{"dotfile", ".bashrc", ".bashrc", ""},
		{"multi-dot", "archive.tar.gz", "archive.tar", ".gz"},
		{"no-ext", "Makefile", "Makefile", ""},
		{"hidden-multi-dot", ".config.toml", ".config", ".toml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stem, ext := conflictStemExt(tt.input)
			if stem != tt.wantStem || ext != tt.wantExt {
				t.Errorf("conflictStemExt(%q) = (%q, %q), want (%q, %q)", tt.input, stem, ext, tt.wantStem, tt.wantExt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Retry tests
// ---------------------------------------------------------------------------

func TestWithRetry_Succeeds(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(context.Background(), "test-op", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_RetriesOnTransient(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(context.Background(), "test-retry", func() error {
		calls++
		if calls < 3 {
			return graph.ErrThrottled
		}

		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_NoRetryOnSkip(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(context.Background(), "test-skip", func() error {
		calls++
		return graph.ErrForbidden
	})

	if !errors.Is(err, graph.ErrForbidden) {
		t.Errorf("expected ErrForbidden, got %v", err)
	}

	if calls != 1 {
		t.Errorf("expected 1 call (no retry), got %d", calls)
	}
}

// Fix 8: Test retry exhaustion — all attempts return retryable error.
func TestWithRetry_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(context.Background(), "exhaust", func() error {
		calls++
		return graph.ErrThrottled
	})

	if !errors.Is(err, graph.ErrThrottled) {
		t.Errorf("expected ErrThrottled, got %v", err)
	}

	// executorMaxRetries=3 → 1 initial + 3 retries = 4 total.
	if calls != executorMaxRetries+1 {
		t.Errorf("expected %d calls, got %d", executorMaxRetries+1, calls)
	}
}

// Fix 9: Test conflict download-failure restore path.
func TestExecutor_Conflict_DownloadFails_RestoresLocal(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	originalContent := "precious local data"
	writeExecTestFile(t, syncRoot, "exec-restore.txt", originalContent)

	action := &Action{
		Type:    ActionConflict,
		Path:    "exec-restore.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Remote: &RemoteState{ItemID: "item1", ParentID: "root"},
		},
		ConflictInfo: &ConflictRecord{ConflictType: "edit_edit"},
	}

	o := e.executeConflict(context.Background(), action)
	requireOutcomeFailure(t, o)

	// Original file should be restored after download failure.
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-restore.txt"))
	if err != nil {
		t.Fatalf("original file should have been restored: %v", err)
	}

	if string(data) != originalContent {
		t.Errorf("expected restored content %q, got %q", originalContent, string(data))
	}
}

// Fix 10: Test executeRemoteMove API error.
func TestExecutor_RemoteMove_Error(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Path: "renamed.txt"},
	}

	o := e.executeMove(context.Background(), action)
	requireOutcomeFailure(t, o)

	if !errors.Is(o.Error, graph.ErrForbidden) {
		t.Errorf("expected ErrForbidden in outcome error, got %v", o.Error)
	}
}

// Fix 11: Test moveOutcome View field propagation.
func TestExecutor_LocalMove_ViewFields(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-src.txt", "content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "exec-dst.txt",
		OldPath: "exec-src.txt",
		ItemID:  "item1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Path: "exec-dst.txt",
			Remote: &RemoteState{
				Hash:     "remotehash",
				Size:     42,
				ETag:     "etag-move",
				ItemType: ItemTypeFile,
			},
			Local: &LocalState{
				Hash:  "localhash",
				Mtime: 9876543210,
			},
		},
	}

	o := e.executeMove(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.RemoteHash != "remotehash" {
		t.Errorf("expected RemoteHash=remotehash, got %s", o.RemoteHash)
	}

	if o.Size != 42 {
		t.Errorf("expected Size=42, got %d", o.Size)
	}

	if o.ETag != "etag-move" {
		t.Errorf("expected ETag=etag-move, got %s", o.ETag)
	}

	if o.LocalHash != "localhash" {
		t.Errorf("expected LocalHash=localhash, got %s", o.LocalHash)
	}

	if o.Mtime != 9876543210 {
		t.Errorf("expected Mtime=9876543210, got %d", o.Mtime)
	}

	if o.ItemType != ItemTypeFile {
		t.Errorf("expected ItemType=file, got %s", o.ItemType)
	}
}

// Fix 12: Test large-file upload delegates to Uploader with correct size.
func TestExecutor_Upload_LargeFileSizePassedToUploader(t *testing.T) {
	t.Parallel()

	var capturedSize int64

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedSize = size
			return &graph.Item{ID: "multi-chunk1", ETag: "mc1", QuickXorHash: "h1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	// 25 MiB file — Uploader receives the exact size.
	expectedSize := int64(25 * 1024 * 1024)
	bigContent := strings.Repeat("x", int(expectedSize))
	writeExecTestFile(t, syncRoot, "exec-multi-chunk.bin", bigContent)

	action := &Action{
		Type: ActionUpload,
		Path: "exec-multi-chunk.bin",
		View: &PathView{Path: "exec-multi-chunk.bin"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if capturedSize != expectedSize {
		t.Errorf("expected size=%d, got %d", expectedSize, capturedSize)
	}
}

// Test timeSleep context cancellation (consolidated from timeSleepExec, B-106).
func TestTimeSleep_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := timeSleep(ctx, 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// Test timeSleep completes normally (consolidated from timeSleepExec, B-106).
func TestTimeSleep_Completes(t *testing.T) {
	t.Parallel()

	err := timeSleep(context.Background(), 1*time.Millisecond)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ItemType propagation tests (Fixes 3, 4, 5)
// ---------------------------------------------------------------------------

func TestExecutor_DeleteOutcome_FolderType(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error { return nil },
	}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-folder-del",
		ItemID:  "folder1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		},
	}

	o := e.executeRemoteDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if o.ItemType != ItemTypeFolder {
		t.Errorf("expected ItemType=folder, got %s", o.ItemType)
	}
}

func TestExecutor_Cleanup_FolderType(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionCleanup,
		Path:    "exec-cleanup-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		},
	}

	o := e.executeCleanup(action)
	requireOutcomeSuccess(t, o)

	if o.ItemType != ItemTypeFolder {
		t.Errorf("expected ItemType=folder, got %s", o.ItemType)
	}
}

func TestExecutor_SyncedUpdate_BaselineFallback(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	// No Remote, only Baseline with folder type.
	action := &Action{
		Type:    ActionUpdateSynced,
		Path:    "exec-synced-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(testDriveID),
		View: &PathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
			Local:    &LocalState{Hash: "lh", Mtime: 123},
		},
	}

	o := e.executeSyncedUpdate(action)
	requireOutcomeSuccess(t, o)

	if o.ItemType != ItemTypeFolder {
		t.Errorf("expected ItemType=folder from baseline fallback, got %s", o.ItemType)
	}
}

// ---------------------------------------------------------------------------
// Local delete with trash tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalDelete_TrashSuccess(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	trashCalled := false
	cfg.trashFunc = func(absPath string) error {
		trashCalled = true
		// Simulate successful trash by removing the file.
		return os.Remove(absPath)
	}

	e := NewExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "trash-file.txt", "content")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "trash-file.txt",
		ItemID: "item1",
		View:   &PathView{Baseline: &BaselineEntry{}},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if !trashCalled {
		t.Error("trashFunc should have been called")
	}
}

func TestExecutor_LocalDelete_TrashFailure_FallsBackToRemove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	cfg.trashFunc = func(_ string) error {
		return fmt.Errorf("trash unavailable")
	}

	e := NewExecution(cfg, emptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "trash-fallback.txt", "content")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "trash-fallback.txt",
		ItemID: "item1",
		View:   &PathView{Baseline: &BaselineEntry{}},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	// File should still be deleted (via os.Remove fallback).
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Error("file should have been deleted by os.Remove fallback")
	}
}

func TestExecutor_LocalDelete_NoTrashFunc_DirectRemove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	// trashFunc is nil — should go straight to os.Remove.

	e := NewExecution(cfg, emptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "no-trash.txt", "content")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "no-trash.txt",
		ItemID: "item1",
		View:   &PathView{Baseline: &BaselineEntry{}},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestExecutor_LocalDeleteFolder_TrashSuccess(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	trashCalled := false
	cfg.trashFunc = func(absPath string) error {
		trashCalled = true

		return os.Remove(absPath)
	}

	e := NewExecution(cfg, emptyBaseline())

	if err := os.MkdirAll(filepath.Join(syncRoot, "trash-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "trash-dir",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(context.Background(), action)
	requireOutcomeSuccess(t, o)

	if !trashCalled {
		t.Error("trashFunc should have been called for folder")
	}
}

// ---------------------------------------------------------------------------
// Regression: B-076 — partial file cleaned on download error after write
// ---------------------------------------------------------------------------

// TestExecutor_Download_PartialFileCleanedOnMidStreamError verifies that when a
// download fails mid-stream after writing some bytes, the .partial file is
// removed. Existing tests cover the API error (no bytes written) and success
// paths, but not the "partial write succeeded, then network error" variant.
func TestExecutor_Download_PartialFileCleanedOnMidStreamError(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Write some bytes first (partial content written to disk).
			n, writeErr := w.Write([]byte("partial data"))
			if writeErr != nil {
				return int64(n), writeErr
			}

			// Fail mid-stream — simulates network error after partial write.
			return int64(n), fmt.Errorf("connection reset after partial write")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "partial-cleanup.txt",
		ItemID:  "item-partial",
		DriveID: driveid.New(testDriveID),
		View:    &PathView{Remote: &RemoteState{}},
	}

	o := e.executeDownload(context.Background(), action)
	requireOutcomeFailure(t, o)

	// The .partial file must not remain on disk after the error.
	partialPath := filepath.Join(syncRoot, "partial-cleanup.txt.partial")
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Error(".partial file should be cleaned up on download error, but it still exists")
	}

	// The final file should also not exist.
	if _, err := os.Stat(filepath.Join(syncRoot, "partial-cleanup.txt")); !os.IsNotExist(err) {
		t.Error("final file should not exist after failed download")
	}
}

// ---------------------------------------------------------------------------
// Regression: B-081 — executor propagates file mtime to Uploader
// ---------------------------------------------------------------------------

// TestExecutor_Upload_MtimePassedToUploader verifies that executeUpload reads
// the local file's modification time and passes it to the Uploader.Upload call.
func TestExecutor_Upload_MtimePassedToUploader(t *testing.T) {
	t.Parallel()

	var capturedMtime time.Time

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, mtime time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedMtime = mtime

			return &graph.Item{ID: "up-mtime", ETag: "e1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())

	// Write a file and set a specific mtime.
	writeExecTestFile(t, syncRoot, "mtime-test.txt", "mtime content")

	targetMtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	absPath := filepath.Join(syncRoot, "mtime-test.txt")

	if err := os.Chtimes(absPath, targetMtime, targetMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	action := &Action{
		Type: ActionUpload,
		Path: "mtime-test.txt",
		View: &PathView{Path: "mtime-test.txt"},
	}

	o := e.executeUpload(context.Background(), action)
	requireOutcomeSuccess(t, o)

	// Verify the uploader received the file's mtime.
	if !capturedMtime.Equal(targetMtime) {
		t.Errorf("uploader received mtime %v, want %v", capturedMtime, targetMtime)
	}

	// Verify the outcome also records the mtime.
	if o.Mtime != targetMtime.UnixNano() {
		t.Errorf("outcome Mtime = %d, want %d", o.Mtime, targetMtime.UnixNano())
	}
}
