package syncexec

import (
	"context"
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
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	execHelloWorldContent      = "hello world"
	execHelloWorldQuickXorHash = "aCgDG9jwBhDc4Q1yawMZAAAAAAA="
)

// ---------------------------------------------------------------------------
// Mock types (prefixed to avoid collision with other test files)
// ---------------------------------------------------------------------------

type executorMockItemClient = testMockItemClient

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
	uploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	uploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *executorMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

func (m *executorMockUploader) UploadToItem(ctx context.Context, driveID driveid.ID, itemID string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadToItemFn != nil {
		return m.uploadToItemFn(ctx, driveID, itemID, content, size, mtime, progress)
	}
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, "", "", content, size, mtime, progress)
	}

	return nil, fmt.Errorf("UploadToItem not mocked")
}

type executorPathConvergenceStub struct {
	waitItem                 *graph.Item
	waitErr                  error
	waitCalls                []string
	deleteResolvedCalls      []string
	permanentDeletePathCalls []string
	targets                  []executorPathConvergenceTarget
}

type executorPathConvergenceTarget struct {
	driveID    driveid.ID
	rootItemID string
}

func (s *executorPathConvergenceStub) ForTarget(driveID driveid.ID, rootItemID string) driveops.PathConvergence {
	s.targets = append(s.targets, executorPathConvergenceTarget{
		driveID:    driveID,
		rootItemID: rootItemID,
	})
	return s
}

func (s *executorPathConvergenceStub) WaitPathVisible(_ context.Context, remotePath string) (*graph.Item, error) {
	s.waitCalls = append(s.waitCalls, remotePath)
	if s.waitErr != nil {
		return nil, s.waitErr
	}
	if s.waitItem != nil {
		return s.waitItem, nil
	}

	return &graph.Item{ID: "visible-item-id"}, nil
}

func (s *executorPathConvergenceStub) DeleteResolvedPath(_ context.Context, remotePath, _ string) error {
	s.deleteResolvedCalls = append(s.deleteResolvedCalls, remotePath)
	return nil
}

func (s *executorPathConvergenceStub) PermanentDeleteResolvedPath(_ context.Context, remotePath, _ string) error {
	s.permanentDeletePathCalls = append(s.permanentDeletePathCalls, remotePath)
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestExecutorConfig(t *testing.T, items *executorMockItemClient, dl *executorMockDownloader, ul *executorMockUploader) (*ExecutorConfig, string) {
	t.Helper()

	return newTestExecutorConfigWithPathConvergence(t, items, dl, ul, nil)
}

func newTestExecutorConfigWithPathConvergence(
	t *testing.T,
	items *executorMockItemClient,
	dl *executorMockDownloader,
	ul *executorMockUploader,
	pathConvergenceFactory driveops.PathConvergenceFactory,
) (*ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, pathConvergenceFactory)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })

	return cfg, syncRoot
}

func writeExecTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	absPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o700))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600))

	return absPath
}

func requireOutcomeSuccess(t *testing.T, o *synctypes.Outcome) {
	t.Helper()

	require.True(t, o.Success, "expected success but got error: %v", o.Error)
}

func requireOutcomeFailure(t *testing.T, o *synctypes.Outcome) {
	t.Helper()

	require.False(t, o.Success, "expected failure but got success")
}

func TestNewExecutorConfig_AllowsNilPathConvergenceFactory(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	require.Nil(t, cfg.pathConvergenceFactory)
}

// ---------------------------------------------------------------------------
// Folder create tests
// ---------------------------------------------------------------------------

func TestExecutor_CreateLocalFolder(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "docs/notes",
		CreateSide: synctypes.CreateLocal,
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{
				ItemID:   "folder1",
				ParentID: "root",
				Mtime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	info, err := os.Stat(filepath.Join(syncRoot, "docs", "notes"))
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "photos",
		CreateSide: synctypes.CreateRemote,
		View:       &synctypes.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "new-folder-id", o.ItemID)
}

func TestExecutor_CreateRemoteFolder_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   "shortcut-parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "photos",
		CreateSide: synctypes.CreateRemote,
		View:       &synctypes.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	require.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_PathConvergenceWarningIsNonFatal(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: driveops.ErrPathNotVisible}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "photos",
		CreateSide: synctypes.CreateRemote,
		View:       &synctypes.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_CrossDriveParentUsesTargetScopedPathConvergence(t *testing.T) {
	t.Parallel()

	const shortcutParent = "shortcut-parent-id"

	var capturedDriveID driveid.ID

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, driveID driveid.ID, parentID, _ string) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, shortcutParent, parentID)
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   shortcutParent,
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	action := &synctypes.Action{
		Type:                synctypes.ActionFolderCreate,
		Path:                "shortcut/photos",
		CreateSide:          synctypes.CreateRemote,
		TargetRootItemID:    shortcutParent,
		TargetRootLocalPath: "shortcut",
		View:                &synctypes.PathView{Path: "shortcut/photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
	assert.Equal(t, []executorPathConvergenceTarget{{
		driveID:    driveid.New("00000000000000ff"),
		rootItemID: shortcutParent,
	}}, pathConvergence.targets)
	assert.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_Error(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "restricted",
		CreateSide: synctypes.CreateRemote,
		View:       &synctypes.PathView{Path: "restricted"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
}

// ---------------------------------------------------------------------------
// Move tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "old-name.txt", "content")

	action := &synctypes.Action{
		Type:    synctypes.ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View:    &synctypes.PathView{Path: "new-name.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.NoFileExists(t, filepath.Join(syncRoot, "old-name.txt"), "old path still exists")
	assert.FileExists(t, filepath.Join(syncRoot, "new-name.txt"), "new path not created")
}

func TestExecutor_LocalMove_SourceMissing(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionLocalMove,
		Path:    "target.txt",
		OldPath: "nonexistent.txt",
		View:    &synctypes.PathView{Path: "target.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "renamed.txt", o.Path)
	assert.Equal(t, "original.txt", o.OldPath)
}

func TestExecutor_RemoteMove_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   "shortcut-parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	action := &synctypes.Action{
		Type:    synctypes.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"renamed.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_RemoteMove_CrossDriveUsesTargetScopedPathConvergence(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   "shortcut-parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	action := &synctypes.Action{
		Type:                synctypes.ActionRemoteMove,
		Path:                "shortcut/renamed.txt",
		OldPath:             "shortcut/original.txt",
		ItemID:              "item1",
		DriveID:             driveid.New("00000000000000ff"),
		TargetRootItemID:    "shortcut-root-id",
		TargetRootLocalPath: "shortcut",
		View:                &synctypes.PathView{Path: "shortcut/renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, []executorPathConvergenceTarget{{
		driveID:    driveid.New("00000000000000ff"),
		rootItemID: "shortcut-root-id",
	}}, pathConvergence.targets)
	assert.Equal(t, []string{"renamed.txt"}, pathConvergence.waitCalls)
}

// ---------------------------------------------------------------------------
// Download tests
// ---------------------------------------------------------------------------

// Validates: R-5.1
func TestExecutor_Download_Success(t *testing.T) {
	t.Parallel()

	execFileContent := execHelloWorldContent

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte(execFileContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "greetings.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				ETag:     "etag1",
				Mtime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "greetings.txt"))
	require.NoError(t, err, "file not created")
	assert.Equal(t, execFileContent, string(data))

	// Partial file should not exist.
	assert.NoFileExists(t, filepath.Join(syncRoot, "greetings.txt.partial"), ".partial file still exists")
	assert.Equal(t, int64(len(execFileContent)), o.LocalSize)
	assert.True(t, o.LocalSizeKnown)
}

func TestExecutor_Download_APIError(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "exec-forbidden.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "deep/nested/dir/exec-dl.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.FileExists(t, filepath.Join(syncRoot, "deep", "nested", "dir", "exec-dl.txt"), "file not created in nested dir")
}

func TestExecutor_Download_ZeroByte(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "exec-empty.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	info, err := os.Stat(filepath.Join(syncRoot, "exec-empty.txt"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "expected zero-byte file")
}

// ---------------------------------------------------------------------------
// Download hash mismatch tests (B-132)
// ---------------------------------------------------------------------------

// Validates: R-5.1
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

			n, err := w.Write([]byte(execHelloWorldContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	correctHash := execHelloWorldQuickXorHash // QuickXorHash of hello world.
	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "hash-retry.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, 3, callCount, "expected 3 download calls")
	assert.Equal(t, correctHash, o.LocalHash)
	assert.Equal(t, correctHash, o.RemoteHash)

	// File should contain correct content.
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "hash-retry.txt"))
	require.NoError(t, err)
	assert.Equal(t, execHelloWorldContent, string(data))
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "hash-accept.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{Hash: "stale-remote-hash", Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// All retries exhausted: 1 initial + 2 retries = 3.
	assert.Equal(t, 3, callCount, "expected 3 download calls")

	// After exhaustion, remoteHash is overridden to localHash to prevent baseline mismatch loop.
	assert.Equal(t, o.LocalHash, o.RemoteHash, "LocalHash should equal RemoteHash after exhaustion")
}

func TestExecutor_Download_HashMatch_NoRetry(t *testing.T) {
	t.Parallel()

	callCount := 0
	content := execHelloWorldContent

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			n, err := w.Write([]byte(content))
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	correctHash := execHelloWorldQuickXorHash
	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "hash-ok.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, 1, callCount, "expected 1 download call")
	assert.Equal(t, correctHash, o.LocalHash)
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

// Validates: R-5.1
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "uploaded1", o.ItemID)
	assert.Equal(t, "root", o.ParentID)
}

func TestExecutor_Upload_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_PathConvergenceProbeFailureIsNonFatal(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: fmt.Errorf("metadata probe failed")}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_CrossDriveParentUsesTargetScopedPathConvergence(t *testing.T) {
	t.Parallel()

	const shortcutParent = "shortcut-parent-id"

	var capturedDriveID driveid.ID

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, driveID driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, shortcutParent, parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   shortcutParent,
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "shortcut/exec-small.txt", "hello")

	action := &synctypes.Action{
		Type:                synctypes.ActionUpload,
		Path:                "shortcut/exec-small.txt",
		TargetRootItemID:    shortcutParent,
		TargetRootLocalPath: "shortcut",
		View:                &synctypes.PathView{Path: "shortcut/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
	assert.Equal(t, []executorPathConvergenceTarget{{
		driveID:    driveid.New("00000000000000ff"),
		rootItemID: shortcutParent,
	}}, pathConvergence.targets)
	assert.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_CrossDriveWithoutTargetRootMetadataSkipsPathConvergence(t *testing.T) {
	t.Parallel()

	const shortcutParent = "shortcut-parent-id"

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, shortcutParent, parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "shortcut",
		ItemID:   shortcutParent,
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: synctypes.ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "shortcut/exec-small.txt", "hello")

	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "shortcut/exec-small.txt",
		View: &synctypes.PathView{Path: "shortcut/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Empty(t, pathConvergence.targets)
	assert.Empty(t, pathConvergence.waitCalls)
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
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "exec-existing-dir",
		ItemID:   "baseline-folder-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: synctypes.ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "exec-existing-dir/exec-doc.txt", "content")

	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "exec-existing-dir/exec-doc.txt",
		View: &synctypes.PathView{Path: "exec-existing-dir/exec-doc.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "baseline-folder-id", capturedParentID)
}

func TestExecutor_Upload_KnownItemUsesItemOverwrite(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "parent-path upload should not be used when item ID is known")
			return nil, fmt.Errorf("unexpected Upload call")
		},
		uploadToItemFn: func(_ context.Context, _ driveid.ID, itemID string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "known-item-id", itemID)

			return &graph.Item{
				ID:           "known-item-id",
				ParentID:     "parent-from-item",
				ETag:         "etag-overwrite",
				QuickXorHash: "overwrite-hash",
			}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "known.txt",
		ItemID:   "known-item-id",
		ParentID: "baseline-parent",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: synctypes.ItemTypeFile,
	}))

	writeExecTestFile(t, syncRoot, "known.txt", "known content")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "known.txt",
		ItemID:  "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "known.txt",
			Baseline: &synctypes.BaselineEntry{
				ParentID: "baseline-parent",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "known-item-id", o.ItemID)
	assert.Equal(t, "parent-from-item", o.ParentID)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-new-file.txt", "data")

	// New local item has zero DriveID (EF13 scenario).
	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "exec-new-file.txt",
		View: &synctypes.PathView{Path: "exec-new-file.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// Executor should have filled driveID from its own context.
	assert.Equal(t, driveid.New(synctest.TestDriveID), capturedDriveID)
}

// Validates: R-5.1
func TestExecutor_Upload_LargeFileSuccess(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "chunked1", ETag: "ce1", QuickXorHash: "hash1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, synctest.EmptyBaseline())

	// Create a file > 4 MiB to exercise Upload for large files.
	bigContent := strings.Repeat("x", 5*1024*1024) // 5 MiB
	writeExecTestFile(t, syncRoot, "exec-big-file.bin", bigContent)

	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "exec-big-file.bin",
		View: &synctypes.PathView{Path: "exec-big-file.bin"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "chunked1", o.ItemID)
}

// ---------------------------------------------------------------------------
// Local delete tests
// ---------------------------------------------------------------------------

// Validates: R-6.2.4
func TestExecutor_LocalDelete_HashMatch(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "exec-delete-me.txt", "content")

	hash, err := driveops.ComputeQuickXorHash(absPath)
	require.NoError(t, err)

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-delete-me.txt",
		ItemID: "item1",
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{LocalHash: hash},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
}

// Validates: R-6.2.4
func TestExecutor_LocalDelete_HashMismatch_ConflictCopy(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-modified.txt", "new content")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// B-133: outcome should be ActionConflict (not ActionLocalDelete) so it's tracked.
	assert.Equal(t, synctypes.ActionConflict, o.Action, "expected ActionConflict")
	assert.Equal(t, synctypes.ConflictEditDelete, o.ConflictType, "expected ConflictEditDelete")
	assert.Equal(t, "baseline-remote-hash", o.RemoteHash)

	// Original should be gone.
	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-modified.txt"))
	assert.True(t, os.IsNotExist(statErr), "original file should have been renamed")

	// Conflict copy should exist.
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	found := false

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			found = true
		}
	}

	assert.True(t, found, "expected conflict copy to be created")
}

// Validates: R-6.2.4
func TestExecutor_LocalDelete_HashMismatch_ConflictCopyCollisionGetsSuffix(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-modified.txt", "new content")
	writeExecTestFile(t, syncRoot, "exec-modified.conflict-20260115-120000.txt", "existing conflict")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, synctypes.ActionConflict, o.Action)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-modified.txt"))
	assert.True(t, os.IsNotExist(statErr), "original file should have been renamed")

	existingData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-modified.conflict-20260115-120000.txt"))
	require.NoError(t, err)
	assert.Equal(t, "existing conflict", string(existingData))

	suffixedData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-modified.conflict-20260115-120000-2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new content", string(suffixedData))
}

func TestExecutor_LocalDelete_AlreadyGone(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-already-gone.txt",
		ItemID: "item1",
		View:   &synctypes.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

func TestExecutor_LocalDelete_FolderEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "exec-empty-dir"), 0o700))

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-empty-dir",
		ItemID: "item1",
		View:   &synctypes.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-empty-dir"))
	assert.True(t, os.IsNotExist(statErr), "directory should have been removed")
}

func TestExecutor_LocalDelete_FolderNotEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-non-empty-dir/child.txt", "data")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "exec-non-empty-dir",
		ItemID: "item1",
		View:   &synctypes.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

func TestExecutor_RemoteDelete_ErrorHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		itemID    string
		deleteErr error
		wantOK    bool
	}{
		{name: "404IsSuccess", path: "exec-already-deleted.txt", itemID: "item2", deleteErr: graph.ErrNotFound, wantOK: true},
		{name: "403Skip", path: "exec-forbidden-del.txt", itemID: "item3", deleteErr: graph.ErrForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			items := &executorMockItemClient{
				deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
					return tt.deleteErr
				},
			}

			cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
			e := NewExecution(cfg, synctest.EmptyBaseline())

			action := &synctypes.Action{
				Type:    synctypes.ActionRemoteDelete,
				Path:    tt.path,
				ItemID:  tt.itemID,
				DriveID: driveid.New(synctest.TestDriveID),
				View:    &synctypes.PathView{},
			}

			o := e.ExecuteRemoteDelete(t.Context(), action)
			if tt.wantOK {
				requireOutcomeSuccess(t, &o)
				return
			}

			requireOutcomeFailure(t, &o)
		})
	}
}

// ---------------------------------------------------------------------------
// Conflict tests
// ---------------------------------------------------------------------------

// Validates: R-2.3.1
func TestExecutor_Conflict_EditEdit_KeepBoth(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")

	action := &synctypes.Action{
		Type:    synctypes.ActionConflict,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				ETag:     "etag1",
			},
		},
		ConflictInfo: &synctypes.ConflictRecord{ConflictType: "edit_edit"},
	}

	o := e.ExecuteConflict(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "edit_edit", o.ConflictType)

	// Original path should have remote content.
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote version", string(data))

	// Conflict copy should have local content.
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	conflictFound := false

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			conflictData, readErr := localpath.ReadFile(filepath.Join(syncRoot, entry.Name()))
			require.NoError(t, readErr)
			if string(conflictData) == "local version" {
				conflictFound = true
			}
		}
	}

	assert.True(t, conflictFound, "expected conflict copy with local content")
}

// Validates: R-2.3.1
func TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")
	writeExecTestFile(t, syncRoot, "exec-conflict.conflict-20260115-120000.txt", "existing conflict")

	action := &synctypes.Action{
		Type:    synctypes.ActionConflict,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				ETag:     "etag1",
			},
		},
		ConflictInfo: &synctypes.ConflictRecord{ConflictType: "edit_edit"},
	}

	o := e.ExecuteConflict(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	originalData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote version", string(originalData))

	existingData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict.conflict-20260115-120000.txt"))
	require.NoError(t, err)
	assert.Equal(t, "existing conflict", string(existingData))

	suffixedData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict.conflict-20260115-120000-2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local version", string(suffixedData))
}

func TestExecutor_Conflict_EditDelete_AutoResolve(t *testing.T) {
	t.Parallel()

	var uploadCalled bool
	var uploadToItemCalled bool

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:   "new-item",
				Name: name,
				ETag: "etag-new",
			}, nil
		},
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadToItemCalled = true
			return nil, fmt.Errorf("unexpected UploadToItem call")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "folder",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "parent-folder",
		ItemType: synctypes.ItemTypeFolder,
	})
	e := NewExecution(cfg, baseline)

	// Local file exists with modified content (edit-delete: local modified,
	// remote deleted).
	writeExecTestFile(t, syncRoot, "folder/exec-ed-file.txt", "locally modified data")

	action := &synctypes.Action{
		Type:    synctypes.ActionConflict,
		Path:    "folder/exec-ed-file.txt",
		ItemID:  "deleted-item",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "folder/exec-ed-file.txt",
			Remote: &synctypes.RemoteState{
				ItemID:    "deleted-item",
				DriveID:   driveid.New(synctest.TestDriveID),
				ParentID:  "parent-folder",
				ItemType:  synctypes.ItemTypeFile,
				IsDeleted: true,
			},
			Baseline: &synctypes.BaselineEntry{
				Path:     "folder/exec-ed-file.txt",
				DriveID:  driveid.New(synctest.TestDriveID),
				ItemID:   "deleted-item",
				ParentID: "parent-folder",
				ItemType: synctypes.ItemTypeFile,
			},
		},
		ConflictInfo: &synctypes.ConflictRecord{
			ConflictType: "edit_delete",
			DriveID:      driveid.New(synctest.TestDriveID),
		},
	}

	o := e.ExecuteConflict(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.True(t, uploadCalled, "expected upload to be called for edit-delete auto-resolve")
	assert.False(t, uploadToItemCalled, "edit-delete auto-resolve should not overwrite a deleted item ID")
	assert.Equal(t, synctypes.ActionConflict, o.Action)
	assert.Equal(t, "edit_delete", o.ConflictType)
	assert.Equal(t, "auto", o.ResolvedBy)
	assert.Equal(t, "new-item", o.ItemID)

	// Local file should still exist with original content (not modified by upload).
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "folder", "exec-ed-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "locally modified data", string(data))
}

// ---------------------------------------------------------------------------
// Conflict copy naming tests
// ---------------------------------------------------------------------------

func TestConflictCopyPath_Normal(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := ConflictCopyPath("/sync/root/exec-file.txt", ts)
	expected := "/sync/root/exec-file.conflict-20260115-123045.txt"

	assert.Equal(t, expected, result)
}

func TestConflictCopyPath_Dotfile(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := ConflictCopyPath("/sync/root/.bashrc", ts)
	expected := "/sync/root/.bashrc.conflict-20260115-123045"

	assert.Equal(t, expected, result)
}

func TestConflictCopyPath_MultiDot(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	result := ConflictCopyPath("/sync/root/archive.tar.gz", ts)
	expected := "/sync/root/archive.tar.conflict-20260115-123045.gz"

	assert.Equal(t, expected, result)
}

// ---------------------------------------------------------------------------
// Synced update tests
// ---------------------------------------------------------------------------

func TestExecutor_SyncedUpdate(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionUpdateSynced,
		Path:    "exec-converged.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{
				ItemID:   "item1",
				ParentID: "root",
				Hash:     "hash1",
				Size:     1024,
				ETag:     "etag1",
				ItemType: synctypes.ItemTypeFile,
			},
			Local: &synctypes.LocalState{
				Hash:  "hash1",
				Mtime: 1234567890,
			},
		},
	}

	o := e.ExecuteSyncedUpdate(action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "hash1", o.RemoteHash)
	assert.Equal(t, "hash1", o.LocalHash)
	assert.Equal(t, int64(1024), o.RemoteSize)
	assert.True(t, o.RemoteSizeKnown)
	assert.Equal(t, int64(1234567890), o.LocalMtime)
}

// ---------------------------------------------------------------------------
// Cleanup tests
// ---------------------------------------------------------------------------

func TestExecutor_Cleanup(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionCleanup,
		Path:    "exec-ghost.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
	}

	o := e.ExecuteCleanup(action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, synctypes.ActionCleanup, o.Action)
	assert.Equal(t, "exec-ghost.txt", o.Path)
}

// ---------------------------------------------------------------------------
// Parent ID resolution tests
// ---------------------------------------------------------------------------

func TestExecutor_ResolveParentID_Baseline(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "exec-existing-folder",
		ItemID:   "folder-id-from-baseline",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: synctypes.ItemTypeFolder,
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

			id, err := e.ResolveParentID(tt.relPath)
			if tt.expectErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedID, id)
		})
	}
}

func TestExecutor_ResolveParentID_SharedScopedRoot(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetRootItemID("shared-root-id")
	e := NewExecution(cfg, synctest.EmptyBaseline())

	id, err := e.ResolveParentID("exec-file.txt")
	require.NoError(t, err)
	assert.Equal(t, "shared-root-id", id)
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

			stem, ext := ConflictStemExt(tt.input)
			assert.Equal(t, tt.wantStem, stem, "stem mismatch for %q", tt.input)
			assert.Equal(t, tt.wantExt, ext, "ext mismatch for %q", tt.input)
		})
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	originalContent := "precious local data"
	writeExecTestFile(t, syncRoot, "exec-restore.txt", originalContent)

	action := &synctypes.Action{
		Type:    synctypes.ActionConflict,
		Path:    "exec-restore.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Remote: &synctypes.RemoteState{ItemID: "item1", ParentID: "root"},
		},
		ConflictInfo: &synctypes.ConflictRecord{ConflictType: "edit_edit"},
	}

	o := e.ExecuteConflict(t.Context(), action)
	requireOutcomeFailure(t, &o)

	// Original file should be restored after download failure.
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-restore.txt"))
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)

	assert.ErrorIs(t, o.Error, graph.ErrForbidden)
}

// Fix 11: Test moveOutcome View field propagation.
func TestExecutor_LocalMove_ViewFields(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-src.txt", "content")

	action := &synctypes.Action{
		Type:    synctypes.ActionLocalMove,
		Path:    "exec-dst.txt",
		OldPath: "exec-src.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "exec-dst.txt",
			Remote: &synctypes.RemoteState{
				Hash:     "remotehash",
				Size:     42,
				ETag:     "etag-move",
				ItemType: synctypes.ItemTypeFile,
			},
			Local: &synctypes.LocalState{
				Hash:  "localhash",
				Mtime: 9876543210,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "remotehash", o.RemoteHash)
	assert.Equal(t, int64(42), o.RemoteSize)
	assert.True(t, o.RemoteSizeKnown)
	assert.Equal(t, "etag-move", o.ETag)
	assert.Equal(t, "localhash", o.LocalHash)
	assert.Equal(t, int64(9876543210), o.LocalMtime)
	assert.Equal(t, synctypes.ItemTypeFile, o.ItemType)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	// 25 MiB file — Uploader receives the exact size.
	expectedSize := int64(25 * 1024 * 1024)
	bigContent := strings.Repeat("x", int(expectedSize))
	writeExecTestFile(t, syncRoot, "exec-multi-chunk.bin", bigContent)

	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "exec-multi-chunk.bin",
		View: &synctypes.PathView{Path: "exec-multi-chunk.bin"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, expectedSize, capturedSize)
}

// Test timeSleep context cancellation (consolidated from timeSleepExec, B-106).
func TestTimeSleep_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := syncobserve.TimeSleep(ctx, 10*time.Second)
	assert.ErrorIs(t, err, context.Canceled)
}

// Test timeSleep completes normally (consolidated from timeSleepExec, B-106).
func TestTimeSleep_Completes(t *testing.T) {
	t.Parallel()

	err := syncobserve.TimeSleep(t.Context(), 1*time.Millisecond)
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionRemoteDelete,
		Path:    "exec-folder-del",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFolder},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, synctypes.ItemTypeFolder, o.ItemType)
}

func TestExecutor_Cleanup_FolderType(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionCleanup,
		Path:    "exec-cleanup-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFolder},
		},
	}

	o := e.ExecuteCleanup(action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, synctypes.ItemTypeFolder, o.ItemType)
}

func TestExecutor_SyncedUpdate_BaselineFallback(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	// No Remote, only Baseline with folder type.
	action := &synctypes.Action{
		Type:    synctypes.ActionUpdateSynced,
		Path:    "exec-synced-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFolder},
			Local:    &synctypes.LocalState{Hash: "lh", Mtime: 123},
		},
	}

	o := e.ExecuteSyncedUpdate(action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, synctypes.ItemTypeFolder, o.ItemType)
}

// ---------------------------------------------------------------------------
// Local delete with trash tests
// ---------------------------------------------------------------------------

// Validates: R-6.4.4
func TestExecutor_LocalDelete_TrashSuccess(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	trashCalled := false
	cfg.SetTrashFunc(func(absPath string) error {
		trashCalled = true
		// Simulate successful trash by removing the file.
		return os.Remove(absPath)
	})

	e := NewExecution(cfg, synctest.EmptyBaseline())

	writeExecTestFile(t, syncRoot, "trash-file.txt", "content")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "trash-file.txt",
		ItemID: "item1",
		View:   &synctypes.PathView{Baseline: &synctypes.BaselineEntry{}},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.True(t, trashCalled, "trashFunc should have been called")
}

// Validates: R-6.4.4
func TestExecutor_LocalDelete_TrashFailure_FallsBackToRemove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	cfg.SetTrashFunc(func(_ string) error {
		return fmt.Errorf("trash unavailable")
	})

	e := NewExecution(cfg, synctest.EmptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "trash-fallback.txt", "content")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "trash-fallback.txt",
		ItemID: "item1",
		View:   &synctypes.PathView{Baseline: &synctypes.BaselineEntry{}},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// File should still be deleted (via os.Remove fallback).
	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted by os.Remove fallback")
}

func TestExecutor_LocalDelete_NoTrashFunc_DirectRemove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	// trashFunc is nil — should go straight to os.Remove.

	e := NewExecution(cfg, synctest.EmptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "no-trash.txt", "content")

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "no-trash.txt",
		ItemID: "item1",
		View:   &synctypes.PathView{Baseline: &synctypes.BaselineEntry{}},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
}

// Validates: R-6.4.4
func TestExecutor_LocalDeleteFolder_TrashSuccess(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	trashCalled := false
	cfg.SetTrashFunc(func(absPath string) error {
		trashCalled = true

		return os.Remove(absPath)
	})

	e := NewExecution(cfg, synctest.EmptyBaseline())

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "trash-dir"), 0o700))

	action := &synctypes.Action{
		Type:   synctypes.ActionLocalDelete,
		Path:   "trash-dir",
		ItemID: "item1",
		View:   &synctypes.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "partial-cleanup.txt",
		ItemID:  "item-partial",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &synctypes.PathView{Remote: &synctypes.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)

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

// TestExecutor_Upload_MtimePassedToUploader verifies that ExecuteUpload reads
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
	e := NewExecution(cfg, synctest.EmptyBaseline())

	// Write a file and set a specific mtime.
	writeExecTestFile(t, syncRoot, "mtime-test.txt", "mtime content")

	targetMtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	absPath := filepath.Join(syncRoot, "mtime-test.txt")

	require.NoError(t, os.Chtimes(absPath, targetMtime, targetMtime))

	action := &synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "mtime-test.txt",
		View: &synctypes.PathView{Path: "mtime-test.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// Verify the uploader received the file's mtime.
	assert.True(t, capturedMtime.Equal(targetMtime), "uploader received mtime %v, want %v", capturedMtime, targetMtime)

	// Verify the outcome also records the mtime.
	assert.Equal(t, targetMtime.UnixNano(), o.LocalMtime)
}

// ---------------------------------------------------------------------------
// Path containment guard tests (B-312)
// ---------------------------------------------------------------------------

func TestContainedPath_ValidPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	tests := []struct {
		name    string
		relPath string
		want    string
	}{
		{"simple file", "file.txt", filepath.Join(root, "file.txt")},
		{"nested path", "dir/subdir/file.txt", filepath.Join(root, "dir", "subdir", "file.txt")},
		{"deep nesting", "a/b/c/d/e.txt", filepath.Join(root, "a", "b", "c", "d", "e.txt")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ContainedPath(root, tt.relPath)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainedPath_TraversalAttempts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

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

			_, err := ContainedPath(root, tt.relPath)
			require.Error(t, err)
			assert.ErrorIs(t, err, synctypes.ErrPathEscapesSyncRoot)
		})
	}
}

func TestCreateLocalFolder_TraversalBlocked(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, synctest.EmptyBaseline())

	action := &synctypes.Action{
		Type:       synctypes.ActionFolderCreate,
		Path:       "../escape",
		CreateSide: synctypes.CreateLocal,
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.ErrorIs(t, o.Error, synctypes.ErrPathEscapesSyncRoot)
}

// ---------------------------------------------------------------------------
// Symlink TOCTOU tests (B-323)
// ---------------------------------------------------------------------------

func TestContainedPath_SymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()

	// Create a symlink inside root that points outside root.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	_, err := ContainedPath(root, "escape/secret.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, synctypes.ErrPathEscapesSyncRoot)
}

func TestContainedPath_SymlinkWithinRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create a real target directory and a symlink to it, both within root.
	target := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(root, "link")))

	got, err := ContainedPath(root, "link/file.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "link", "file.txt"), got)
}

func TestContainedPath_NonexistentPath_StillAllowed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Path doesn't exist on disk — EvalSymlinks will fail, so
	// ContainedPath should fall back to lexical-only (still safe).
	got, err := ContainedPath(root, "does/not/exist.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "does", "not", "exist.txt"), got)
}

func TestContainedPath_MissingSyncRootReturnsError(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "missing-root")

	_, err := ContainedPath(root, "file.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evaluating sync root symlinks")
}

// ---------------------------------------------------------------------------
// Watch-mode upload freshness check tests
// ---------------------------------------------------------------------------

func TestExecutor_Upload_WatchMode_ETagMismatch(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			// Remote has a different eTag — someone else edited the file.
			return &graph.Item{ID: "item1", ETag: "etag-remote-changed"}, nil
		},
	}

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not be called when eTag mismatches")
			return nil, fmt.Errorf("unexpected upload call")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	cfg.SetWatchMode(true)

	e := NewExecution(cfg, synctest.EmptyBaseline())
	writeExecTestFile(t, syncRoot, "conflict.txt", "local content")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "conflict.txt",
			Baseline: &synctypes.BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "remote eTag changed since last sync")
}

func TestExecutor_Upload_WatchMode_ETagMatch(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			// Remote eTag matches baseline — safe to upload.
			return &graph.Item{ID: "item1", ETag: "etag-same"}, nil
		},
	}

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag-new", QuickXorHash: "qxh"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	cfg.SetWatchMode(true)

	e := NewExecution(cfg, synctest.EmptyBaseline())
	writeExecTestFile(t, syncRoot, "safe.txt", "content")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "safe.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "safe.txt",
			Baseline: &synctypes.BaselineEntry{
				ETag: "etag-same",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

func TestExecutor_Upload_NonWatchMode_NoFreshnessCheck(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			require.FailNow(t, "GetItem should not be called in non-watch mode")
			return nil, fmt.Errorf("unexpected GetItem call")
		},
	}

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag1", QuickXorHash: "qxh"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	// cfg.watchMode is false by default — no freshness check.

	e := NewExecution(cfg, synctest.EmptyBaseline())
	writeExecTestFile(t, syncRoot, "normal.txt", "content")

	action := &synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "normal.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &synctypes.PathView{
			Path: "normal.txt",
			Baseline: &synctypes.BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}
