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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o644))

	return absPath
}

func requireOutcomeSuccess(t *testing.T, o Outcome) {
	t.Helper()

	require.True(t, o.Success, "expected success but got error: %v", o.Error)
}

func requireOutcomeFailure(t *testing.T, o Outcome) {
	t.Helper()

	require.False(t, o.Success, "expected failure but got success")
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

	o := e.executeFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, o)

	info, err := os.Stat(filepath.Join(syncRoot, "docs/notes"))
	require.NoError(t, err, "folder not created")
	require.True(t, info.IsDir(), "expected directory")
}

func TestExecutor_CreateRemoteFolder(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			assert.Equal(t, "root", parentID, "unexpected parentID")
			assert.Equal(t, "photos", name, "unexpected name")

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

	o := e.executeFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "new-folder-id", o.ItemID)
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

	o := e.executeFolderCreate(t.Context(), action)
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

	o := e.executeMove(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.NoFileExists(t, filepath.Join(syncRoot, "old-name.txt"), "old path still exists")
	assert.FileExists(t, filepath.Join(syncRoot, "new-name.txt"), "new path not created")
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

	o := e.executeMove(t.Context(), action)
	requireOutcomeFailure(t, o)
}

func TestExecutor_RemoteMove(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, "root", newParentID)
			assert.Equal(t, "renamed.txt", newName)

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

	o := e.executeMove(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "renamed.txt", o.Path)
	assert.Equal(t, "original.txt", o.OldPath)
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	data, err := os.ReadFile(filepath.Join(syncRoot, "greetings.txt"))
	require.NoError(t, err, "file not created")
	assert.Equal(t, execFileContent, string(data))

	// Partial file should not exist.
	assert.NoFileExists(t, filepath.Join(syncRoot, "greetings.txt.partial"), ".partial file still exists")
	assert.Equal(t, int64(len(execFileContent)), o.Size)
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

	o := e.executeDownload(t.Context(), action)
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.FileExists(t, filepath.Join(syncRoot, "deep/nested/dir/exec-dl.txt"), "file not created in nested dir")
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	info, err := os.Stat(filepath.Join(syncRoot, "exec-empty.txt"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "expected zero-byte file")
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, 3, callCount, "expected 3 download calls")
	assert.Equal(t, correctHash, o.LocalHash)
	assert.Equal(t, correctHash, o.RemoteHash)

	// File should contain correct content.
	data, err := os.ReadFile(filepath.Join(syncRoot, "hash-retry.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	// All retries exhausted: 1 initial + 2 retries = 3.
	assert.Equal(t, 3, callCount, "expected 3 download calls")

	// After exhaustion, remoteHash is overridden to localHash to prevent baseline mismatch loop.
	assert.Equal(t, o.LocalHash, o.RemoteHash, "LocalHash should equal RemoteHash after exhaustion")
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, 1, callCount, "expected 1 download call")
	assert.Equal(t, correctHash, o.LocalHash)
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

func TestExecutor_Upload_SimpleSuccess(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "root", parentID)
			assert.Equal(t, "exec-small.txt", name)

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

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "uploaded1", o.ItemID)
	assert.Equal(t, "root", o.ParentID)
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

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "baseline-folder-id", capturedParentID)
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

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	// Executor should have filled driveID from its own context.
	assert.Equal(t, driveid.New(testDriveID), capturedDriveID)
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

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "chunked1", o.ItemID)
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
	require.NoError(t, err)

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-delete-me.txt",
		ItemID: "item1",
		View: &PathView{
			Baseline: &BaselineEntry{LocalHash: hash},
		},
	}

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	// B-133: outcome should be ActionConflict (not ActionLocalDelete) so it's tracked.
	assert.Equal(t, ActionConflict, o.Action, "expected ActionConflict")
	assert.Equal(t, ConflictEditDelete, o.ConflictType, "expected ConflictEditDelete")
	assert.Equal(t, "baseline-remote-hash", o.RemoteHash)

	// Original should be gone.
	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-modified.txt"))
	assert.True(t, os.IsNotExist(statErr), "original file should have been renamed")

	// Conflict copy should exist.
	entries, _ := os.ReadDir(syncRoot)
	found := false

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			found = true
		}
	}

	assert.True(t, found, "expected conflict copy to be created")
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)
}

func TestExecutor_LocalDelete_FolderEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "exec-empty-dir"), 0o755))

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-empty-dir",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-empty-dir"))
	assert.True(t, os.IsNotExist(statErr), "directory should have been removed")
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, o)
}

// ---------------------------------------------------------------------------
// Remote delete tests
// ---------------------------------------------------------------------------

func TestExecutor_RemoteDelete_Success(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			assert.Equal(t, "item1", itemID, "unexpected itemID")

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

	o := e.executeRemoteDelete(t.Context(), action)
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

	o := e.executeRemoteDelete(t.Context(), action)
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

	o := e.executeRemoteDelete(t.Context(), action)
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

	o := e.executeConflict(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "edit_edit", o.ConflictType)

	// Original path should have remote content.
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-conflict.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote version", string(data))

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

	assert.True(t, conflictFound, "expected conflict copy with local content")
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

	o := e.executeConflict(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.True(t, uploadCalled, "expected upload to be called for edit-delete auto-resolve")
	assert.Equal(t, ActionConflict, o.Action)
	assert.Equal(t, "edit_delete", o.ConflictType)
	assert.Equal(t, "auto", o.ResolvedBy)
	assert.Equal(t, "new-item", o.ItemID)

	// Local file should still exist with original content (not modified by upload).
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-ed-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "locally modified data", string(data))
}

// ---------------------------------------------------------------------------
// Conflict copy naming tests
// ---------------------------------------------------------------------------

func TestConflictCopyPath_Normal(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/exec-file.txt", ts)
	expected := "/sync/root/exec-file.conflict-20260115-123045.txt"

	assert.Equal(t, expected, result)
}

func TestConflictCopyPath_Dotfile(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/.bashrc", ts)
	expected := "/sync/root/.bashrc.conflict-20260115-123045"

	assert.Equal(t, expected, result)
}

func TestConflictCopyPath_MultiDot(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := conflictCopyPath("/sync/root/archive.tar.gz", ts)
	expected := "/sync/root/archive.tar.conflict-20260115-123045.gz"

	assert.Equal(t, expected, result)
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

	assert.Equal(t, "hash1", o.RemoteHash)
	assert.Equal(t, "hash1", o.LocalHash)
	assert.Equal(t, int64(1024), o.Size)
	assert.Equal(t, int64(1234567890), o.Mtime)
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

	assert.Equal(t, ActionCleanup, o.Action)
	assert.Equal(t, "exec-ghost.txt", o.Path)
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
		{"404 transient not found via GraphError", &graph.GraphError{StatusCode: 404, Err: graph.ErrNotFound}, errClassRetryable},
		{"509 bandwidth exceeded", &graph.GraphError{StatusCode: 509}, errClassRetryable},
		{"401 via GraphError", &graph.GraphError{StatusCode: 401, Err: graph.ErrUnauthorized}, errClassFatal},
		{"429 via GraphError", &graph.GraphError{StatusCode: 429, Err: graph.ErrThrottled}, errClassRetryable},
		{"500 via GraphError", &graph.GraphError{StatusCode: 500, Err: graph.ErrServerError}, errClassRetryable},
		{"502 via GraphError", &graph.GraphError{StatusCode: 502, Err: graph.ErrServerError}, errClassRetryable},
		{"403 via GraphError", &graph.GraphError{StatusCode: 403, Err: graph.ErrForbidden}, errClassSkip},
		{"423 locked via GraphError", &graph.GraphError{StatusCode: 423, Err: graph.ErrLocked}, errClassSkip},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyError(tt.err)
			assert.Equal(t, tt.expected, got, "classifyError(%v)", tt.err)
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
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedID, id)
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
			assert.Equal(t, tt.wantStem, stem, "stem mismatch for %q", tt.input)
			assert.Equal(t, tt.wantExt, ext, "ext mismatch for %q", tt.input)
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
	err := e.withRetry(t.Context(), "test-op", func() error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestWithRetry_RetriesOnTransient(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(t.Context(), "test-retry", func() error {
		calls++
		if calls < 3 {
			return graph.ErrThrottled
		}

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestWithRetry_NoRetryOnSkip(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(t.Context(), "test-skip", func() error {
		calls++
		return graph.ErrForbidden
	})

	assert.ErrorIs(t, err, graph.ErrForbidden)
	assert.Equal(t, 1, calls, "expected 1 call (no retry)")
}

// Fix 8: Test retry exhaustion — all attempts return retryable error.
func TestWithRetry_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	calls := 0
	err := e.withRetry(t.Context(), "exhaust", func() error {
		calls++
		return graph.ErrThrottled
	})

	assert.ErrorIs(t, err, graph.ErrThrottled)

	// executorMaxRetries=3 -> 1 initial + 3 retries = 4 total.
	assert.Equal(t, executorMaxRetries+1, calls)
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

	o := e.executeConflict(t.Context(), action)
	requireOutcomeFailure(t, o)

	// Original file should be restored after download failure.
	data, err := os.ReadFile(filepath.Join(syncRoot, "exec-restore.txt"))
	require.NoError(t, err, "original file should have been restored")
	assert.Equal(t, originalContent, string(data))
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

	o := e.executeMove(t.Context(), action)
	requireOutcomeFailure(t, o)

	assert.ErrorIs(t, o.Error, graph.ErrForbidden)
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

	o := e.executeMove(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, "remotehash", o.RemoteHash)
	assert.Equal(t, int64(42), o.Size)
	assert.Equal(t, "etag-move", o.ETag)
	assert.Equal(t, "localhash", o.LocalHash)
	assert.Equal(t, int64(9876543210), o.Mtime)
	assert.Equal(t, ItemTypeFile, o.ItemType)
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

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, expectedSize, capturedSize)
}

// Test timeSleep context cancellation (consolidated from timeSleepExec, B-106).
func TestTimeSleep_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := timeSleep(ctx, 10*time.Second)
	assert.ErrorIs(t, err, context.Canceled)
}

// Test timeSleep completes normally (consolidated from timeSleepExec, B-106).
func TestTimeSleep_Completes(t *testing.T) {
	t.Parallel()

	err := timeSleep(t.Context(), 1*time.Millisecond)
	assert.NoError(t, err)
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

	o := e.executeRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.Equal(t, ItemTypeFolder, o.ItemType)
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

	assert.Equal(t, ItemTypeFolder, o.ItemType)
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

	assert.Equal(t, ItemTypeFolder, o.ItemType)
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.True(t, trashCalled, "trashFunc should have been called")
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	// File should still be deleted (via os.Remove fallback).
	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted by os.Remove fallback")
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

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
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

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "trash-dir"), 0o755))

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "trash-dir",
		ItemID: "item1",
		View:   &PathView{},
	}

	o := e.executeLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, o)

	assert.True(t, trashCalled, "trashFunc should have been called for folder")
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

	o := e.executeDownload(t.Context(), action)
	requireOutcomeFailure(t, o)

	// The .partial file must not remain on disk after the error.
	partialPath := filepath.Join(syncRoot, "partial-cleanup.txt.partial")
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), ".partial file should be cleaned up on download error, but it still exists")

	// The final file should also not exist.
	_, statErr2 := os.Stat(filepath.Join(syncRoot, "partial-cleanup.txt"))
	assert.True(t, os.IsNotExist(statErr2), "final file should not exist after failed download")
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

	require.NoError(t, os.Chtimes(absPath, targetMtime, targetMtime))

	action := &Action{
		Type: ActionUpload,
		Path: "mtime-test.txt",
		View: &PathView{Path: "mtime-test.txt"},
	}

	o := e.executeUpload(t.Context(), action)
	requireOutcomeSuccess(t, o)

	// Verify the uploader received the file's mtime.
	assert.True(t, capturedMtime.Equal(targetMtime), "uploader received mtime %v, want %v", capturedMtime, targetMtime)

	// Verify the outcome also records the mtime.
	assert.Equal(t, targetMtime.UnixNano(), o.Mtime)
}

// ---------------------------------------------------------------------------
// Path containment guard tests (B-312)
// ---------------------------------------------------------------------------

func TestContainedPath_ValidPaths(t *testing.T) {
	t.Parallel()

	root := "/sync/root"

	tests := []struct {
		name    string
		relPath string
		want    string
	}{
		{"simple file", "file.txt", "/sync/root/file.txt"},
		{"nested path", "dir/subdir/file.txt", "/sync/root/dir/subdir/file.txt"},
		{"deep nesting", "a/b/c/d/e.txt", "/sync/root/a/b/c/d/e.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := containedPath(root, tt.relPath)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainedPath_TraversalAttempts(t *testing.T) {
	t.Parallel()

	root := "/sync/root"

	tests := []struct {
		name    string
		relPath string
	}{
		{"parent traversal", "../escape.txt"},
		{"deep traversal", "../../etc/passwd"},
		{"mid-path traversal", "subdir/../../escape.txt"},
		{"absolute path", "/etc/passwd"},
		{"empty path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := containedPath(root, tt.relPath)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPathEscapesSyncRoot)
		})
	}
}

func TestCreateLocalFolder_TraversalBlocked(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "../escape",
		CreateSide: CreateLocal,
	}

	o := e.executeFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, o)
	assert.ErrorIs(t, o.Error, ErrPathEscapesSyncRoot)
}
