package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
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
	// rawResponse returns uploadFn's item as is instead of a faithful one,
	// to model a server that stored something other than what was sent.
	rawResponse    bool
	uploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	uploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *executorMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil && m.rawResponse {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}
	if m.uploadFn != nil {
		return faithfulUploadResult(content, size)(m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress))
	}

	return nil, fmt.Errorf("Upload not mocked")
}

func (m *executorMockUploader) UploadToItem(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadToItemFn != nil {
		return faithfulUploadResult(content, size)(m.uploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress))
	}
	if m.uploadFn != nil {
		return faithfulUploadResult(content, size)(m.uploadFn(ctx, driveID, "", "", content, size, mtime, progress))
	}

	return nil, fmt.Errorf("UploadToItem not mocked")
}

// faithfulUploadResult makes a mocked upload answer like a server that
// stored exactly the bytes it was sent: the returned item carries their size
// and QuickXorHash, as driveops verifies the stored item against them.
func faithfulUploadResult(content io.ReaderAt, size int64) func(*graph.Item, error) (*graph.Item, error) {
	return func(item *graph.Item, err error) (*graph.Item, error) {
		if err != nil || item == nil {
			return item, err
		}

		data := make([]byte, size)
		if _, readErr := content.ReadAt(data, 0); readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("reading mocked upload content: %w", readErr)
		}

		h := quickxorhash.New()
		h.Write(data)
		stored := *item
		stored.Size = size
		stored.QuickXorHash = base64.StdEncoding.EncodeToString(h.Sum(nil))

		return &stored, nil
	}
}

type executorPathConvergenceStub struct {
	waitItem                 *graph.Item
	waitErr                  error
	waitFn                   func(remotePath string)
	waitCalls                []string
	deleteResolvedCalls      []string
	permanentDeletePathCalls []string
}

func (s *executorPathConvergenceStub) WaitPathVisible(_ context.Context, remotePath string) (*graph.Item, error) {
	s.waitCalls = append(s.waitCalls, remotePath)
	if s.waitFn != nil {
		s.waitFn(remotePath)
	}
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

func newTestExecutorConfig(t *testing.T, items *executorMockItemClient, dl *executorMockDownloader, ul *executorMockUploader) (*executorConfig, string) {
	t.Helper()

	return newTestExecutorConfigWithPathConvergence(t, items, dl, ul, nil)
}

func newTestExecutorConfigWithPathConvergence(
	t *testing.T,
	items *executorMockItemClient,
	dl *executorMockDownloader,
	ul *executorMockUploader,
	pathConvergence driveops.PathConvergence,
) (*executorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := newExecutorConfig(items, dl, ul, syncTree, driveID, logger, pathConvergence)
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

func requireOutcomeSuccess(t *testing.T, o *actionOutcome) {
	t.Helper()

	require.True(t, o.Success, "expected success but got error: %v", o.Error)
}

func requireOutcomeFailure(t *testing.T, o *actionOutcome) {
	t.Helper()

	require.False(t, o.Success, "expected failure but got success")
}

func TestNewExecutorConfig_AllowsNilPathConvergence(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})

	require.Nil(t, cfg.pathConvergence)
}

// ---------------------------------------------------------------------------
// Folder create tests
// ---------------------------------------------------------------------------

func TestExecutor_CreateLocalFolder(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "docs/notes",
		CreateSide: createLocal,
		View: &pathView{
			Remote: &remoteState{
				ItemID: "folder1",
				Mtime:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		View:       &pathView{Path: "photos"},
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
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "shared",
		ItemID:   "shared-parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: ItemTypeFolder,
	}))

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		View:       &pathView{Path: "photos"},
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		View:       &pathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_WaitsForParentVisibilityBeforeCreate(t *testing.T) {
	t.Parallel()

	pathConvergence := &executorPathConvergenceStub{}
	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "child", name)
			assert.Equal(t, []string{"parent"}, pathConvergence.waitCalls)
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "parent",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	}))

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "parent/child",
		CreateSide: CreateRemote,
		View:       &pathView{Path: "parent/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"parent", "parent/child"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10
func TestExecutor_CreateRemoteFolder_MissingParentPreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "parent-id", itemID)
			return nil, graph.ErrNotFound
		},
		createFolderFn: func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
			require.FailNow(t, "folder create should not run after stale parent preflight")
			return nil, assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "parent",
		ItemID:   "parent-id",
		DriveID:  driveID,
		ItemType: ItemTypeFolder,
	}))

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "parent/child",
		CreateSide: CreateRemote,
		DriveID:    driveID,
		View:       &pathView{Path: "parent/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

func TestExecutor_CreateRemoteFolder_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	var capturedDriveID driveid.ID

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, driveID driveid.ID, parentID, _ string) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, sharedParent, parentID)
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	cfg.SetRemoteRootItemID(sharedParent)
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		DriveID:    driveid.New("00000000000000ff"),
		View:       &pathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "restricted",
		CreateSide: CreateRemote,
		View:       &pathView{Path: "restricted"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability)
}

// ---------------------------------------------------------------------------
// Move tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "old-name.txt", "content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View:    &pathView{Path: "new-name.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.NoFileExists(t, filepath.Join(syncRoot, "old-name.txt"), "old path still exists")
	assert.FileExists(t, filepath.Join(syncRoot, "new-name.txt"), "new path not created")
}

// Validates: R-2.12.4
func TestExecutor_LocalMove_SpellingOnlyRenameOntoSameInodeIsAllowed(t *testing.T) {
	t.Parallel()

	// A case-insensitive volume reports "EDIT.txt" present when only
	// "edit.txt" exists: both names resolve to one inode. A hard link
	// reproduces that on a case-sensitive test filesystem. The remote
	// case-only rename must not be refused as "destination exists".
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	coexist := namesCoexist(t, "edit.txt", "EDIT.txt")
	writeExecTestFile(t, syncRoot, "edit.txt", "content")
	if coexist {
		require.NoError(t, os.Link(filepath.Join(syncRoot, "edit.txt"), filepath.Join(syncRoot, "EDIT.txt")))
	}

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "EDIT.txt",
		OldPath: "edit.txt",
		View:    &pathView{Path: "EDIT.txt"},
	}

	require.NoError(t, e.validateLocalMovePrecondition(action))
}

// Validates: R-2.12.4
func TestExecutor_LocalMove_SpellingOnlyRenameOntoDifferentFileIsStale(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	skipUnlessNamesCoexist(t, "edit.txt", "EDIT.txt")

	writeExecTestFile(t, syncRoot, "edit.txt", "content")
	writeExecTestFile(t, syncRoot, "EDIT.txt", "other content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "EDIT.txt",
		OldPath: "edit.txt",
		View:    &pathView{Path: "EDIT.txt"},
	}

	require.ErrorIs(t, e.validateLocalMovePrecondition(action), errActionPreconditionChanged)
}

// Validates: R-2.12.4
func TestExecutor_LocalMove_SpellingOnlyRenameLeavesNoTemporarySibling(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "stra\u00dfe.txt", "content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "STRASSE.txt",
		OldPath: "stra\u00dfe.txt",
		View:    &pathView{Path: "STRASSE.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{"STRASSE.txt"}, names)
}

func TestExecutor_LocalMove_SourceMissing(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "target.txt",
		OldPath: "nonexistent.txt",
		View:    &pathView{Path: "target.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

// Validates: R-2.8.10, R-2.17.7
//
// OneDrive renamed a file the user edited locally (D-053). The rename still
// applies and carries the local bytes; the outcome keeps the baseline's
// content facts, so the next pass sees the local edit at the new name and
// uploads it. Refusing the move superseded it on every pass and stranded the
// edit under the old name.
func TestExecutor_LocalMove_LocallyEditedSourceStillMovesAndKeepsTheEditPending(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "old-name.txt", "new content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View: &pathView{
			Path: "new-name.txt",
			Baseline: &BaselineEntry{
				Path:      "old-name.txt",
				ItemType:  ItemTypeFile,
				LocalHash: "hash-before-the-local-edit",
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.NoFileExists(t, filepath.Join(syncRoot, "old-name.txt"))
	data, err := os.ReadFile(filepath.Join(syncRoot, "new-name.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "new content", string(data), "the rename carries the local edit")
	assert.Equal(t, "hash-before-the-local-edit", o.LocalHash,
		"the outcome must not claim the edit is synced")
}

// Validates: R-2.8.10
//
// A local move never fabricates its destination's parent (D-054): planning
// orders it after whatever puts the parent in place, so a missing parent is
// stale truth, not something to create.
func TestExecutor_LocalMove_MissingDestinationParentIsStale(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "a.txt", "a")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "missing-parent/a.txt",
		OldPath: "a.txt",
		View: &pathView{
			Path:     "missing-parent/a.txt",
			Baseline: &BaselineEntry{Path: "a.txt", ItemType: ItemTypeFile},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.FileExists(t, filepath.Join(syncRoot, "a.txt"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "missing-parent"))
}

// Validates: R-2.8.10
func TestExecutor_LocalMove_FolderIdentityChangedReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	oldPath := filepath.Join(syncRoot, "old-folder")
	newPath := filepath.Join(syncRoot, "new-folder")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	currentIdentity, err := e.syncTree.IdentityNoFollow("old-folder")
	require.NoError(t, err)

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "new-folder",
		OldPath: "old-folder",
		View: &pathView{
			Baseline: &BaselineEntry{
				ItemType:         ItemTypeFolder,
				LocalDevice:      currentIdentity.Device,
				LocalInode:       currentIdentity.Inode ^ 1,
				LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)
	assert.DirExists(t, oldPath)
	assert.NoDirExists(t, newPath)
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "renamed.txt", o.Path)
	assert.Equal(t, "original.txt", o.OldPath)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_StaleSourcePreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    "etag-current",
			}, nil
		},
		moveItemFn: func(context.Context, driveid.ID, string, string, string) (*graph.Item, error) {
			require.FailNow(t, "remote move should not run after stale source preflight")
			return nil, assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "renamed.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     "etag-planned",
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_UsesConditionalETagFromPreflight(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		moveItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, graphRootID, newParentID)
			assert.Equal(t, "renamed.txt", newName)
			assert.Equal(t, `"etag-current"`, ifMatch)
			return &graph.Item{ID: "item1", ETag: `"etag-next"`}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "renamed.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, `"etag-next"`, o.ETag)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_ConditionalMismatchReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		moveItemIfMatchFn: func(context.Context, driveid.ID, string, string, string, string) (*graph.Item, error) {
			return nil, graph.ErrPreconditionFailed
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "renamed.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability)
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
	e := newExecution(cfg, emptyBaseline())
	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"renamed.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_RemoteMove_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, &executorMockUploader{}, pathConvergence)
	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New("00000000000000ff"),
		View:    &pathView{Path: "renamed.txt"},
	}

	cfg.SetRemoteRootItemID("mount-root-id")
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "greetings.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Remote: &remoteState{
				ItemID: "item1",
				ETag:   "etag1",
				Mtime:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "greetings.txt"))
	require.NoError(t, err, "file not created")
	assert.Equal(t, execFileContent, string(data))

	// Partial file should not exist.
	assert.NoFileExists(t, filepath.Join(syncRoot, ".onedrive-go.greetings.txt.partial"), ".partial file still exists")
	assert.Equal(t, int64(len(execFileContent)), o.LocalSize)
	assert.True(t, o.LocalSizeKnown)
}

// Validates: R-2.8.10
func TestExecutor_Download_MountRootAllowsGraphDriveRootPath(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:              "item1",
				DriveID:         driveID,
				Name:            "shortcut-sentinel.txt",
				ParentPath:      "Kikkeli Shared Test Folder",
				ParentPathKnown: true,
				Size:            int64(len(execHelloWorldContent)),
			}, nil
		},
	}
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte(execHelloWorldContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, dl, &executorMockUploader{})
	cfg.SetRemoteRootItemID("mount-root-id")
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "shortcut-sentinel.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "shortcut-sentinel.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				Size:     int64(len(execHelloWorldContent)),
			},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.FileExists(t, filepath.Join(syncRoot, "shortcut-sentinel.txt"))
}

func TestExecutor_Download_APIError(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "exec-forbidden.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, permissionCapabilityRemoteRead, o.FailureCapability)
}

// Validates: R-5.10.3
func TestExecutor_Download_MissingParentIsStaleAndNotCreated(t *testing.T) {
	t.Parallel()

	var downloads int
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloads++
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	// The planned parent "A" was renamed away locally after planning.
	action := &Action{
		Type:    ActionDownload,
		Path:    "A/exec-dl.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)

	// It used to re-create an empty A/ holding the download, splitting the
	// user's renamed folder in two.
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Zero(t, downloads)
	assert.NoDirExists(t, filepath.Join(syncRoot, "A"))
}

// Validates: R-5.10.3
func TestExecutor_Download_ParentSwappedForSymlinkAfterCheckStaysInRoot(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	moved := filepath.Join(outside, "moved")
	var syncRoot string
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Mid-download, after every check: the parent folder moves out
			// of the sync root and a symlink to it takes its place.
			parent := filepath.Join(syncRoot, "A")
			require.NoError(t, os.Rename(parent, moved))
			require.NoError(t, os.Symlink(moved, parent))

			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, root := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	syncRoot = root
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "A"), 0o700))
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteDownload(t.Context(), &Action{
		Type:    ActionDownload,
		Path:    "A/escape.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Mtime: 1}},
	})

	// Absolute paths followed the swapped symlink, and the download was
	// renamed into place outside the sync root.
	requireOutcomeFailure(t, &o)
	assert.NoFileExists(t, filepath.Join(moved, "escape.txt"), "nothing may be published outside the sync root")
}

func TestExecutor_Download_ZeroByte(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "exec-empty.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	info, err := os.Stat(filepath.Join(syncRoot, "exec-empty.txt"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "expected zero-byte file")
}

// Validates: R-2.8.10
func TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
			}, nil
		},
	}
	var syncRoot string
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote content"))
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "appeared.txt"), []byte("local drift"), 0o600))
			return int64(n), err
		},
	}

	cfg, root := newTestExecutorConfig(t, items, dl, &executorMockUploader{})
	syncRoot = root
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "appeared.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path:   "appeared.txt",
			Remote: &remoteState{DriveID: driveID, ItemID: "item1", ItemType: ItemTypeFile},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "appeared.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local drift", string(data))
	assert.FileExists(t, filepath.Join(syncRoot, ".onedrive-go.appeared.txt.partial"))
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
	e := newExecution(cfg, emptyBaseline())

	correctHash := execHelloWorldQuickXorHash // QuickXorHash of hello world.
	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-retry.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Hash: correctHash, Mtime: 1}},
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

// Validates: R-5.5.1, R-5.8.2
func TestExecutor_Download_HashMismatch_FailsWithoutWritingTarget(t *testing.T) {
	t.Parallel()

	callCount := 0
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			// An error page served with status 200, every time.
			n, err := w.Write([]byte(`<html>Service unavailable</html>`))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-reject.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Hash: "remote-hash", Size: 4096, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)

	// It used to be accepted after three attempts and renamed into place,
	// with the local hash recorded as the remote one.
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, driveops.ErrDownloadIntegrity)
	assert.Equal(t, 3, callCount, "1 initial + 2 retries")
	assert.Equal(t, issueDownloadIntegrity, issueTypeForResult(&actionCompletion{Err: o.Error}))

	assert.NoFileExists(t, filepath.Join(syncRoot, "hash-reject.txt"), "the error page is never renamed into place")
}

// Validates: R-5.8.2
func TestExecutor_Download_DeterministicTransformRecordsRemoteHash(t *testing.T) {
	t.Parallel()

	served := []byte("served bytes")
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(served)
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteDownload(t.Context(), &Action{
		Type:    ActionDownload,
		Path:    "transformed.docx",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Hash: "reported-hash", Size: int64(len(served)), Mtime: 1}},
	})
	requireOutcomeSuccess(t, &o)

	// Each side records what it has: OneDrive's hash as the remote half,
	// the bytes' hash as the local half, so neither looks changed next pass.
	assert.Equal(t, "reported-hash", o.RemoteHash)
	assert.Equal(t, hashContentQuickXor(t, string(served)), o.LocalHash)
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
	e := newExecution(cfg, emptyBaseline())

	correctHash := execHelloWorldQuickXorHash
	action := &Action{
		Type:    ActionDownload,
		Path:    "hash-ok.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Hash: correctHash, Mtime: 1}},
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
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "uploaded1", o.ItemID)
	assert.Equal(t, "root", o.ParentID)
}

// Validates: R-5.5.2, R-2.5.7
func TestExecuteUpload_ResponseSizeMismatch_DoesNotCommitBaseline(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		rawResponse: true,
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			// A Personal-drive answer that stored 2 bytes of a 5-byte file
			// and carries no hash: size is the only evidence, and it differs.
			return &graph.Item{ID: "short", Name: name, Size: 2, ParentDriveType: "personal"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "short.txt", "hello")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type:    ActionUpload,
		Path:    "short.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "short.txt"},
	})

	// Before: a success outcome recording local=5 bytes and remote=2 bytes
	// as one converged baseline row, never differing from either half again.
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, driveops.ErrUploadVerificationFailed)
	assert.Equal(t, issueUploadVerification, issueTypeForResult(&actionCompletion{Err: o.Error}))
}

func TestExecutor_Upload_APIError(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability)
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
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_CreateByParentWaitsForParentVisibilityBeforeUpload(t *testing.T) {
	t.Parallel()

	pathConvergence := &executorPathConvergenceStub{}
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "exec-small.txt", name)
			assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"folder", "folder/exec-small.txt"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10, R-2.16.2
func TestExecutor_Upload_CreateByParentWaitsBeforeParentPrecondition(t *testing.T) {
	t.Parallel()

	parentWaited := false
	getItemCalls := 0
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			require.Equal(t, driveid.New(synctest.TestDriveID), driveID)
			require.Equal(t, "parent-id", itemID)
			getItemCalls++
			if !parentWaited {
				return nil, graph.ErrNotFound
			}
			return &graph.Item{ID: "parent-id", DriveID: driveID, IsFolder: true}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{
		waitFn: func(remotePath string) {
			if remotePath == "folder" {
				parentWaited = true
			}
		},
	}
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.True(t, parentWaited, "parent path convergence should run before parent preflight and upload")
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "exec-small.txt", name)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, ul, pathConvergence)
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, 1, getItemCalls)
	assert.Equal(t, []string{"folder", "folder/exec-small.txt"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.7, R-2.8.10
func TestExecutor_Upload_CreateByParentMissingAfterVisibilityWaitReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			require.Equal(t, driveid.New(synctest.TestDriveID), driveID)
			require.Equal(t, "parent-id", itemID)
			return nil, graph.ErrNotFound
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: driveops.ErrPathNotVisible}
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not start when parent preflight proves stale work")
			return nil, assert.AnError
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, ul, pathConvergence)
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteRead, o.FailureCapability)
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls)
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
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_CrossDriveParentUsesTargetScopedPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	var capturedDriveID driveid.ID

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, driveID driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, sharedParent, parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	cfg.SetRemoteRootItemID(sharedParent)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New("00000000000000ff"),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
	assert.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_Upload_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, sharedParent, parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &executorMockItemClient{}, &executorMockDownloader{}, ul, pathConvergence)
	cfg.SetRemoteRootItemID(sharedParent)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &Action{
		Type:    ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New("00000000000000ff"),
		View:    &pathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"exec-small.txt"}, pathConvergence.waitCalls)
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
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "exec-existing-dir",
		ItemID:   "baseline-folder-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	}))

	writeExecTestFile(t, syncRoot, "exec-existing-dir/exec-doc.txt", "content")

	action := &Action{
		Type: ActionUpload,
		Path: "exec-existing-dir/exec-doc.txt",
		View: &pathView{Path: "exec-existing-dir/exec-doc.txt"},
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
		uploadToItemFn: func(_ context.Context, _ driveid.ID, itemID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
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
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "known.txt",
		ItemID:   "known-item-id",
		ParentID: "baseline-parent",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFile,
	}))

	writeExecTestFile(t, syncRoot, "known.txt", "known content")

	action := &Action{
		Type:    ActionUpload,
		Path:    "known.txt",
		ItemID:  "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "known.txt",
			Baseline: &BaselineEntry{
				ParentID: "baseline-parent",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "known-item-id", o.ItemID)
	assert.Equal(t, "parent-from-item", o.ParentID)
}

func TestExecutor_Upload_ZeroDriveIDIsFilled(t *testing.T) {
	t.Parallel()

	var capturedDriveID driveid.ID

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, driveID driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			return &graph.Item{ID: "up1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-new-file.txt", "data")

	// New local item has zero DriveID until execution binds the mounted drive.
	action := &Action{
		Type: ActionUpload,
		Path: "exec-new-file.txt",
		View: &pathView{Path: "exec-new-file.txt"},
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
	e := newExecution(cfg, emptyBaseline())

	// Create a file > 4 MiB to exercise Upload for large files.
	bigContent := strings.Repeat("x", 5*1024*1024) // 5 MiB
	writeExecTestFile(t, syncRoot, "exec-big-file.bin", bigContent)

	action := &Action{
		Type: ActionUpload,
		Path: "exec-big-file.bin",
		View: &pathView{Path: "exec-big-file.bin"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "chunked1", o.ItemID)
}

// Validates: R-2.8.10
func TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not start after local source precondition changes")
			return nil, assert.AnError
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "changed-before-upload.txt", "local drift")

	action := &Action{
		Type: ActionUpload,
		Path: "changed-before-upload.txt",
		View: &pathView{
			Path: "changed-before-upload.txt",
			Local: &localState{
				ItemType: ItemTypeFile,
				Hash:     execHelloWorldQuickXorHash,
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalRead, o.FailureCapability)
}

// Validates: R-2.4.6, R-2.8.10
func TestExecutor_Upload_SymlinkAliasSourceUsesFollowedTargetPrecondition(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "alias.txt", name)
			assert.Equal(t, int64(len(execHelloWorldContent)), size)
			buf := make([]byte, size)
			_, err := content.ReadAt(buf, 0)
			require.NoError(t, err)
			assert.Equal(t, execHelloWorldContent, string(buf))
			return &graph.Item{ID: "uploaded-alias", ETag: "etag1", QuickXorHash: execHelloWorldQuickXorHash}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())

	targetPath := writeExecTestFile(t, syncRoot, "target.txt", execHelloWorldContent)
	aliasPath := filepath.Join(syncRoot, "alias.txt")
	if err := os.Symlink(targetPath, aliasPath); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}
	info, err := os.Stat(aliasPath)
	require.NoError(t, err)
	identity, identityOK := synctree.IdentityFromFileInfo(info)
	require.True(t, identityOK)

	action := &Action{
		Type:    ActionUpload,
		Path:    "alias.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "alias.txt",
			Local: &localState{
				ItemType:         ItemTypeFile,
				Hash:             execHelloWorldQuickXorHash,
				Size:             info.Size(),
				Mtime:            info.ModTime().UnixNano(),
				LocalDevice:      identity.Device,
				LocalInode:       identity.Inode,
				LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

// ---------------------------------------------------------------------------
// Local delete tests
// ---------------------------------------------------------------------------

// Validates: R-6.2.4
func TestExecutor_LocalDelete_HashMatch(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	absPath := writeExecTestFile(t, syncRoot, "exec-delete-me.txt", "content")

	hash, err := driveops.ComputeQuickXorHash(absPath)
	require.NoError(t, err)

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-delete-me.txt",
		ItemID: "item1",
		View: &pathView{
			Baseline: &BaselineEntry{LocalHash: hash},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
}

// Validates: R-6.2.4, R-2.8.10
func TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	uploadCalled := false
	uploader := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			assert.Equal(t, graphRootID, parentID)
			assert.Equal(t, "exec-modified.txt", name)
			return &graph.Item{ID: "uploaded-item", ETag: "etag1", QuickXorHash: "remote-hash"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, uploader)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-modified.txt", "new content")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &pathView{
			Baseline: &BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	require.False(t, o.Success)
	assert.Equal(t, ActionLocalDelete, o.Action)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)

	contents, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-modified.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new content", string(contents), "local file should remain in place")

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".conflict-", "edit-delete recreate upload should not create a conflict copy")
	}
	assert.False(t, uploadCalled, "executor should not invent upload intent for stale local delete")
}

// Validates: R-6.2.4
func TestExecutor_LocalDelete_HashMismatch_DoesNotCreateConflictCopy(t *testing.T) {
	t.Parallel()

	uploadCalled := false
	uploader := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			assert.Equal(t, graphRootID, parentID)
			assert.Equal(t, "exec-modified.txt", name)
			return &graph.Item{ID: "uploaded-item", ETag: "etag1", QuickXorHash: "remote-hash"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, uploader)
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-modified.txt", "new content")
	writeExecTestFile(t, syncRoot, "exec-modified.conflict-20260115-120000.txt", "existing conflict")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &pathView{
			Baseline: &BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	require.False(t, o.Success)
	assert.Equal(t, ActionLocalDelete, o.Action)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)

	currentData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-modified.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new content", string(currentData))

	existingData, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-modified.conflict-20260115-120000.txt"))
	require.NoError(t, err)
	assert.Equal(t, "existing conflict", string(existingData))

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-modified.conflict-20260115-120000-2.txt"))
	assert.True(t, os.IsNotExist(statErr), "stale local delete should not create a suffixed conflict copy")
	assert.False(t, uploadCalled, "executor should not invent upload intent for stale local delete")
}

// Validates: R-2.8.10
func TestExecutor_LocalDelete_AlreadyGone(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-already-gone.txt",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)
}

func TestExecutor_LocalDelete_FolderEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "exec-empty-dir"), 0o700))

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-empty-dir",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-empty-dir"))
	assert.True(t, os.IsNotExist(statErr), "directory should have been removed")
}

// Validates: R-2.8.10
func TestExecutor_LocalDelete_FolderIdentityChangedReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	dirPath := filepath.Join(syncRoot, "exec-folder-recreated")
	require.NoError(t, os.MkdirAll(dirPath, 0o700))
	currentIdentity, err := e.syncTree.IdentityNoFollow("exec-folder-recreated")
	require.NoError(t, err)

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-folder-recreated",
		ItemID: "item1",
		View: &pathView{
			Baseline: &BaselineEntry{
				ItemType:         ItemTypeFolder,
				LocalDevice:      currentIdentity.Device,
				LocalInode:       currentIdentity.Inode ^ 1,
				LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)
	assert.DirExists(t, dirPath)
}

func TestExecutor_LocalDelete_FolderNotEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-non-empty-dir/child.txt", "data")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "exec-non-empty-dir",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	outside := t.TempDir()
	outsideFile := writeExecTestFile(t, outside, "target/file.txt", "outside data")
	aliasPath := filepath.Join(syncRoot, "alias")
	if err := os.Symlink(filepath.Join(outside, "target"), aliasPath); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "alias/file.txt",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "won't delete alias/file.txt: alias is a symbolic link")
	assert.FileExists(t, outsideFile)
	assert.FileExists(t, filepath.Join(syncRoot, "alias", "file.txt"))
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

// Validates: R-2.8.10
func TestExecutor_RemoteDelete_UsesConditionalETagFromPreflight(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, ifMatch string) error {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, `"etag-current"`, ifMatch)
			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "exec-remote-file.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
}

// Validates: R-2.8.10
func TestExecutor_RemoteDelete_ConditionalMismatchReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		deleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			return graph.ErrPreconditionFailed
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "exec-remote-file.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_RemoteDelete_WrongDrivePreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	plannedDriveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveid.New("other-drive"),
				ETag:    `"etag-current"`,
			}, nil
		},
		deleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			require.FailNow(t, "remote delete should not run after wrong-drive preflight")
			return assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: plannedDriveID,
		View: &pathView{
			Path: "exec-remote-file.txt",
			Remote: &remoteState{
				DriveID:  plannedDriveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteRead, o.FailureCapability)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_RemoteDelete_StalePathPreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:              "item1",
				DriveID:         driveID,
				Name:            "file.txt",
				ParentPath:      "Moved",
				ParentPathKnown: true,
				ETag:            `"etag-current"`,
			}, nil
		},
		deleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			require.FailNow(t, "remote delete should not run after stale-path preflight")
			return assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "Docs/file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &pathView{
			Path: "Docs/file.txt",
			Remote: &remoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityRemoteRead, o.FailureCapability)
}

// Validates: R-2.8.10
func TestExecutor_RemoteDelete_ErrorHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		path                  string
		itemID                string
		deleteErr             error
		wantOK                bool
		wantErrIs             error
		wantFailurePath       string
		wantFailureCapability permissionCapability
	}{
		{
			name:                  "404AfterPreflightIsSuperseded",
			path:                  "exec-already-deleted.txt",
			itemID:                "item2",
			deleteErr:             graph.ErrNotFound,
			wantErrIs:             errActionPreconditionChanged,
			wantFailurePath:       "exec-already-deleted.txt",
			wantFailureCapability: permissionCapabilityRemoteWrite,
		},
		{
			name:                  "403Skip",
			path:                  "exec-forbidden-del.txt",
			itemID:                "item3",
			deleteErr:             graph.ErrForbidden,
			wantErrIs:             graph.ErrForbidden,
			wantFailurePath:       "exec-forbidden-del.txt",
			wantFailureCapability: permissionCapabilityRemoteWrite,
		},
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
			e := newExecution(cfg, emptyBaseline())

			action := &Action{
				Type:    ActionRemoteDelete,
				Path:    tt.path,
				ItemID:  tt.itemID,
				DriveID: driveid.New(synctest.TestDriveID),
				View:    &pathView{},
			}

			o := e.ExecuteRemoteDelete(t.Context(), action)
			if tt.wantOK {
				requireOutcomeSuccess(t, &o)
				return
			}

			requireOutcomeFailure(t, &o)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, o.Error, tt.wantErrIs)
			}
			assert.Equal(t, tt.wantFailurePath, o.FailurePath)
			assert.Equal(t, tt.wantFailureCapability, o.FailureCapability)
		})
	}
}

// ---------------------------------------------------------------------------
// Conflict tests
// ---------------------------------------------------------------------------

// Validates: R-2.3.1, R-2.3.5
func TestExecutor_Conflict_EditEdit_KeepBoth(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")

	copyAction := &Action{
		Type:    ActionConflictCopy,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Local: &localState{
				ItemType: ItemTypeFile,
			},
			Remote: &remoteState{
				ItemID: "item1",
				ETag:   "etag1",
			},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	requireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	requireOutcomeSuccess(t, &o)

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
func TestExecutor_ConflictDownload_TargetReappearsAfterConflictCopyReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	downloadCalled := false
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			downloadCalled = true
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict-reappears.txt", "original local")

	copyAction := &Action{
		Type:    ActionConflictCopy,
		Path:    "exec-conflict-reappears.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Local:  &localState{ItemType: ItemTypeFile},
			Remote: &remoteState{ItemID: "item1"},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	requireOutcomeSuccess(t, &copyOutcome)

	writeExecTestFile(t, syncRoot, "exec-conflict-reappears.txt", "fresh local")

	downloadAction := *copyAction
	downloadAction.Type = ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)
	assert.False(t, downloadCalled)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict-reappears.txt"))
	require.NoError(t, err)
	assert.Equal(t, "fresh local", string(data))
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
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")
	writeExecTestFile(t, syncRoot, "exec-conflict.conflict-20260115-120000.txt", "existing conflict")

	copyAction := &Action{
		Type:    ActionConflictCopy,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Remote: &remoteState{
				ItemID: "item1",
				ETag:   "etag1",
			},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	requireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
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

func TestExecutor_Conflict_EditDelete_RecreatesRemoteFromLocal(t *testing.T) {
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
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadToItemCalled = true
			return nil, fmt.Errorf("unexpected UploadToItem call")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	baseline := baselineWith(&BaselineEntry{
		Path:     "folder",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "parent-folder",
		ItemType: ItemTypeFolder,
	})
	e := newExecution(cfg, baseline)

	// Local file exists with modified content (edit-delete: local modified,
	// remote deleted).
	writeExecTestFile(t, syncRoot, "folder/exec-ed-file.txt", "locally modified data")

	action := &Action{
		Type:    ActionUpload,
		Path:    "folder/exec-ed-file.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "folder/exec-ed-file.txt",
			Remote: &remoteState{
				ItemID:    "deleted-item",
				DriveID:   driveid.New(synctest.TestDriveID),
				ItemType:  ItemTypeFile,
				IsDeleted: true,
			},
			Baseline: &BaselineEntry{
				Path:     "folder/exec-ed-file.txt",
				DriveID:  driveid.New(synctest.TestDriveID),
				ItemID:   "deleted-item",
				ParentID: "parent-folder",
				ItemType: ItemTypeFile,
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.True(t, uploadCalled, "expected edit-delete handling to recreate the remote file")
	assert.False(t, uploadToItemCalled, "edit-delete handling should not overwrite a deleted item ID")
	assert.Equal(t, ActionUpload, o.Action)
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
	result := conflictCopyPath("/sync/root/exec-file.txt", ts)
	expected := "/sync/root/exec-file.conflict-20260115-123045.txt"

	assert.Equal(t, expected, result)
}

// Validates: R-5.2.5
func TestConflictCopyPath_LongNameStaysWithinLocalLimit(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	name := strings.Repeat("w", 246) + ".txt" // 250 bytes
	result := filepath.Base(conflictCopyPath("/sync/root/"+name, ts))
	assert.LessOrEqual(t, len(result), maxLocalNameBytes)
	assert.True(t, strings.HasSuffix(result, ".conflict-20260115-123045.txt"), result)

	multibyte := strings.Repeat("日", 80) + ".md" // 243 bytes
	result = filepath.Base(conflictCopyPath("/sync/root/"+multibyte, ts))
	assert.LessOrEqual(t, len(result), maxLocalNameBytes)
	assert.True(t, utf8.ValidString(result))
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
// Publication-only action tests
// ---------------------------------------------------------------------------

func TestPublicationMutation_SyncedUpdate(t *testing.T) {
	t.Parallel()

	action := &Action{
		Type:    ActionBaselineUpdate,
		Path:    "exec-converged.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Remote: &remoteState{
				ItemID:   "item1",
				Hash:     "hash1",
				Size:     1024,
				ETag:     "etag1",
				ItemType: ItemTypeFile,
			},
			Local: &localState{
				Hash:  "hash1",
				Mtime: 1234567890,
			},
		},
	}

	mutation, err := publicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, "hash1", mutation.RemoteHash)
	assert.Equal(t, "hash1", mutation.LocalHash)
	assert.Equal(t, int64(1024), mutation.RemoteSize)
	assert.True(t, mutation.RemoteSizeKnown)
	assert.Equal(t, int64(1234567890), mutation.LocalMtime)
}

// ---------------------------------------------------------------------------
// Cleanup tests
// ---------------------------------------------------------------------------

func TestPublicationMutation_Cleanup(t *testing.T) {
	t.Parallel()

	action := &Action{
		Type:    ActionCleanup,
		Path:    "exec-ghost.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
	}

	mutation, err := publicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, ActionCleanup, mutation.Action)
	assert.Equal(t, "exec-ghost.txt", mutation.Path)
}

// ---------------------------------------------------------------------------
// Parent ID resolution tests
// ---------------------------------------------------------------------------

func TestExecutor_ResolveParentID_Baseline(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, baselineWith(&BaselineEntry{
		Path:     "exec-existing-folder",
		ItemID:   "folder-id-from-baseline",
		DriveID:  driveid.New(synctest.TestDriveID),
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

func TestExecutor_ResolveParentID_MountRoot(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetRemoteRootItemID("mount-root-id")
	e := newExecution(cfg, emptyBaseline())

	id, err := e.ResolveParentID("exec-file.txt")
	require.NoError(t, err)
	assert.Equal(t, "mount-root-id", id)
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

// Fix 9: Test concrete conflict actions leave the preserved local copy in place
// when the dependent download fails.
func TestExecutor_ConflictDownloadFails_LeavesConflictCopy(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	originalContent := "precious local data"
	writeExecTestFile(t, syncRoot, "exec-restore.txt", originalContent)

	copyAction := &Action{
		Type:    ActionConflictCopy,
		Path:    "exec-restore.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Local:  &localState{ItemType: ItemTypeFile},
			Remote: &remoteState{ItemID: "item1"},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	requireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	requireOutcomeFailure(t, &o)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-restore.txt"))
	assert.True(t, os.IsNotExist(statErr), "canonical path should remain absent until the retrying download succeeds")

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)

	conflictFound := false
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".conflict-") {
			continue
		}
		conflictData, readErr := localpath.ReadFile(filepath.Join(syncRoot, entry.Name()))
		require.NoError(t, readErr)
		if string(conflictData) == originalContent {
			conflictFound = true
		}
	}
	assert.True(t, conflictFound, "preserved local content should remain in the conflict copy after download failure")
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)

	require.ErrorIs(t, o.Error, graph.ErrForbidden)
	assert.Equal(t, action.OldPath, o.FailurePath)
	assert.Equal(t, permissionCapabilityRemoteWrite, o.FailureCapability)
}

func TestInferFailureCapabilityFromError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		err              error
		localCapability  permissionCapability
		remoteCapability permissionCapability
		want             permissionCapability
	}{
		{
			name:             "wrapped local permission",
			err:              fmt.Errorf("opening local file: %w", os.ErrPermission),
			localCapability:  permissionCapabilityLocalRead,
			remoteCapability: permissionCapabilityRemoteWrite,
			want:             permissionCapabilityLocalRead,
		},
		{
			name:             "wrapped remote forbidden",
			err:              fmt.Errorf("uploading remote file: %w", graph.ErrForbidden),
			localCapability:  permissionCapabilityLocalRead,
			remoteCapability: permissionCapabilityRemoteWrite,
			want:             permissionCapabilityRemoteWrite,
		},
		{
			name:             "non permission error",
			err:              fmt.Errorf("something else"),
			localCapability:  permissionCapabilityLocalRead,
			remoteCapability: permissionCapabilityRemoteWrite,
			want:             permissionCapabilityUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := inferFailureCapabilityFromError(tt.err, tt.localCapability, tt.remoteCapability)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Fix 11: Test moveOutcome View field propagation.
func TestExecutor_LocalMove_ViewFields(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-src.txt", "content")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "exec-dst.txt",
		OldPath: "exec-src.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "exec-dst.txt",
			Remote: &remoteState{
				Hash:     "remotehash",
				Size:     42,
				ETag:     "etag-move",
				ItemType: ItemTypeFile,
			},
			// What sat at the destination before the move (here a file the
			// move replaces) is not the moved item's local truth.
			Local: &localState{
				Hash:  "replaced-hash",
				Mtime: 1,
			},
			Baseline: &BaselineEntry{
				ItemType:   ItemTypeFile,
				LocalHash:  "localhash",
				LocalMtime: 9876543210,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, "remotehash", o.RemoteHash)
	assert.Equal(t, int64(42), o.RemoteSize)
	assert.True(t, o.RemoteSizeKnown)
	assert.Equal(t, "etag-move", o.ETag)
	assert.Equal(t, "localhash", o.LocalHash, "the rename preserves the source's synced content")
	assert.Equal(t, int64(9876543210), o.LocalMtime)
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
	e := newExecution(cfg, emptyBaseline())

	// 25 MiB file — Uploader receives the exact size.
	expectedSize := int64(25 * 1024 * 1024)
	bigContent := strings.Repeat("x", int(expectedSize))
	writeExecTestFile(t, syncRoot, "exec-multi-chunk.bin", bigContent)

	action := &Action{
		Type: ActionUpload,
		Path: "exec-multi-chunk.bin",
		View: &pathView{Path: "exec-multi-chunk.bin"},
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
		listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
			return nil, nil
		},
	}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteDelete,
		Path:    "exec-folder-del",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	assert.Equal(t, ItemTypeFolder, o.ItemType)
}

func TestPublicationMutation_Cleanup_FolderType(t *testing.T) {
	t.Parallel()

	action := &Action{
		Type:    ActionCleanup,
		Path:    "exec-cleanup-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		},
	}

	mutation, err := publicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, ItemTypeFolder, mutation.ItemType)
}

func TestPublicationMutation_SyncedUpdate_BaselineFallback(t *testing.T) {
	t.Parallel()

	// No Remote, only Baseline with folder type.
	action := &Action{
		Type:    ActionBaselineUpdate,
		Path:    "exec-synced-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
			Local:    &localState{Hash: "lh", Mtime: 123},
		},
	}

	mutation, err := publicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, ItemTypeFolder, mutation.ItemType)
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
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionDownload,
		Path:    "partial-cleanup.txt",
		ItemID:  "item-partial",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)

	// The .partial file must not remain on disk after the error.
	partialPath := filepath.Join(syncRoot, ".onedrive-go.partial-cleanup.txt.partial")
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
	e := newExecution(cfg, emptyBaseline())

	// Write a file and set a specific mtime.
	writeExecTestFile(t, syncRoot, "mtime-test.txt", "mtime content")

	targetMtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	absPath := filepath.Join(syncRoot, "mtime-test.txt")

	require.NoError(t, os.Chtimes(absPath, targetMtime, targetMtime))

	action := &Action{
		Type: ActionUpload,
		Path: "mtime-test.txt",
		View: &pathView{Path: "mtime-test.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// Verify the uploader received the file's mtime.
	assert.True(t, capturedMtime.Equal(targetMtime), "uploader received mtime %v, want %v", capturedMtime, targetMtime)

	// Verify the outcome also records the mtime.
	assert.Equal(t, targetMtime.UnixNano(), o.LocalMtime)
}

// Validates: R-6.4.8
func TestCreateLocalFolder_TraversalBlocked(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "../escape",
		CreateSide: createLocal,
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.ErrorIs(t, o.Error, errPathEscapesSyncRoot)
}

// ---------------------------------------------------------------------------
// Upload freshness check tests
// ---------------------------------------------------------------------------

func TestExecutor_Upload_ETagMismatch(t *testing.T) {
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

	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "conflict.txt", "local content")

	action := &Action{
		Type:    ActionUpload,
		Path:    "conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "conflict.txt",
			Baseline: &BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Contains(t, o.Error.Error(), "upload overwrite remote item item1 changed since planning")
}

func TestExecutor_Upload_AlwaysChecksFreshnessWhenBaselineETagKnown(t *testing.T) {
	t.Parallel()

	getItemCalls := 0
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: "item1", ETag: "etag-baseline"}, nil
		},
	}

	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag1", QuickXorHash: "qxh"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)

	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "normal.txt", "content")

	action := &Action{
		Type:    ActionUpload,
		Path:    "normal.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "normal.txt",
			Baseline: &BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, 1, getItemCalls, "expected one freshness probe before upload")
}

// moveTestFileBaseline builds the source baseline a real sync would have
// recorded for relPath, so local move preconditions pass against it.
func moveTestFileBaseline(t *testing.T, e *executor, syncRoot, relPath string) *BaselineEntry {
	t.Helper()

	info, err := os.Stat(filepath.Join(syncRoot, relPath))
	require.NoError(t, err)
	hash, err := e.hashFunc(filepath.Join(syncRoot, relPath))
	require.NoError(t, err)
	identity, err := e.syncTree.IdentityNoFollow(relPath)
	require.NoError(t, err)

	return &BaselineEntry{
		Path:             relPath,
		ItemID:           "item-moved",
		ItemType:         ItemTypeFile,
		LocalHash:        hash,
		LocalSize:        info.Size(),
		LocalSizeKnown:   true,
		LocalMtime:       info.ModTime().UnixNano(),
		LocalDevice:      identity.Device,
		LocalInode:       identity.Inode,
		LocalHasIdentity: true,
		RemoteHash:       "remote-hash",
		RemoteSize:       info.Size(),
		RemoteSizeKnown:  true,
		RemoteMtime:      11,
		ETag:             "etag-before-move",
	}
}

// Validates: R-2.8.10, R-2.17.3
func TestExecutor_LocalMove_PublishesSourceBaselineLocalHalf(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "renamed.txt", "hi")
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "sub"), 0o750))
	baseline := moveTestFileBaseline(t, e, syncRoot, "renamed.txt")

	// A server-side rename with unchanged content: only path and eTag move.
	action := &Action{
		Type:    ActionLocalMove,
		Path:    "sub/renamed-remotely.txt",
		OldPath: "renamed.txt",
		ItemID:  "item-moved",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "sub/renamed-remotely.txt",
			Remote: &remoteState{
				ItemID: "item-moved", ItemType: ItemTypeFile,
				Hash: "remote-hash", Size: baseline.RemoteSize, Mtime: 11, ETag: "etag-after-move",
			},
			Baseline: baseline,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// Without the source baseline's local half the moved row lost its local
	// hash and identity, so the next scan saw a change and re-uploaded.
	assert.Equal(t, baseline.LocalHash, o.LocalHash)
	assert.Equal(t, baseline.LocalSize, o.LocalSize)
	assert.True(t, o.LocalSizeKnown)
	assert.Equal(t, baseline.LocalMtime, o.LocalMtime)
	moved, err := e.syncTree.IdentityNoFollow("sub/renamed-remotely.txt")
	require.NoError(t, err)
	assert.True(t, o.LocalHasIdentity)
	assert.Equal(t, moved.Inode, o.LocalInode)
	assert.Equal(t, "remote-hash", o.RemoteHash)
	assert.Equal(t, "etag-after-move", o.ETag)
}

// Validates: R-2.17.3
func TestExecutor_LocalMove_RemoteEditKeepsBaselineRemoteContent(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "doc.txt", "old")
	baseline := moveTestFileBaseline(t, e, syncRoot, "doc.txt")

	action := &Action{
		Type:    ActionLocalMove,
		Path:    "moved-doc.txt",
		OldPath: "doc.txt",
		ItemID:  "item-moved",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "moved-doc.txt",
			Remote: &remoteState{
				ItemID: "item-moved", ItemType: ItemTypeFile,
				Hash: "edited-remote-hash", Size: 99, Mtime: 12, ETag: "etag-edited",
			},
			Baseline: baseline,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// The local copy still holds the old content, so the baseline must keep
	// describing the old remote content; the next pass then downloads the edit.
	assert.Equal(t, "remote-hash", o.RemoteHash)
	assert.Equal(t, baseline.RemoteSize, o.RemoteSize)
	assert.Equal(t, "etag-before-move", o.ETag)
	assert.Equal(t, baseline.LocalHash, o.LocalHash)
}

// Validates: R-2.17.3
func TestExecutor_RemoteMove_PublishesObservedDestinationLocalTruth(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-swap", ETag: "etag-moved"}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "secret.txt",
		OldPath: "vim.swp",
		ItemID:  "item-swap",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: "vim.swp",
			Baseline: &BaselineEntry{
				Path: "vim.swp", ItemID: "item-swap", ItemType: ItemTypeFile,
				LocalHash: "swap-hash", LocalSize: 5, LocalSizeKnown: true, LocalMtime: 100,
				LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
				RemoteHash: "swap-hash", RemoteSize: 5, RemoteSizeKnown: true,
			},
		},
		MovedLocal: &localState{
			ItemType: ItemTypeFile, Size: 5, Mtime: 100,
			LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	// The destination was never hashed; the baseline must not claim the
	// source's hash for it.
	assert.Empty(t, o.LocalHash)
	assert.Equal(t, int64(5), o.LocalSize)
	assert.Equal(t, int64(100), o.LocalMtime)
	assert.True(t, o.LocalIdentityObserved)
	assert.Equal(t, uint64(8), o.LocalInode)
	assert.Equal(t, "swap-hash", o.RemoteHash)
}

// Validates: R-2.17.1
func TestExecutor_CreateLocalFolder_RecordsFilesystemIdentity(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "deep/er",
		CreateSide: createLocal,
		ItemID:     "item-er",
		DriveID:    driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path:   "deep/er",
			Remote: &remoteState{ItemID: "item-er", ItemType: ItemTypeFolder},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	identity, err := e.syncTree.IdentityNoFollow("deep/er")
	require.NoError(t, err)
	assert.True(t, o.LocalIdentityObserved)
	assert.True(t, o.LocalHasIdentity, "folder baselines need identity so a later local rename pairs as one move")
	assert.Equal(t, identity.Device, o.LocalDevice)
	assert.Equal(t, identity.Inode, o.LocalInode)
}

// Validates: R-2.17.1
func TestExecutor_CreateRemoteFolder_RecordsObservedLocalIdentity(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			return &graph.Item{ID: "item-" + name, Name: name, IsFolder: true}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "deep",
		CreateSide: CreateRemote,
		DriveID:    driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path:  "deep",
			Local: &localState{ItemType: ItemTypeFolder, LocalDevice: 3, LocalInode: 4, LocalHasIdentity: true},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.True(t, o.LocalHasIdentity)
	assert.Equal(t, uint64(4), o.LocalInode)
}

// Validates: R-2.8.10, R-2.17.1
func TestExecutor_RemoteMove_FolderETagChurnFromChildrenDoesNotSupersede(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	var sentIfMatch string
	items := &executorMockItemClient{
		// The folder's eTag moved on when children were uploaded into it
		// after its baseline/remote row was recorded.
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-dir", DriveID: driveID, IsFolder: true, ETag: `"etag-after-children"`}, nil
		},
		moveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, ifMatch string) (*graph.Item, error) {
			sentIfMatch = ifMatch
			return &graph.Item{ID: "item-dir", IsFolder: true, ETag: `"etag-moved"`}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "movedst",
		OldPath: "movesrc",
		ItemID:  "item-dir",
		DriveID: driveID,
		View: &pathView{
			Path: "movesrc",
			Remote: &remoteState{
				DriveID: driveID, ItemID: "item-dir", ItemType: ItemTypeFolder, ETag: `"etag-at-create"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Empty(t, sentIfMatch, "a folder move must not be conditional on a churning folder eTag")
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_FileETagMismatchStillSupersedes(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-file", DriveID: driveID, ETag: `"etag-changed"`}, nil
		},
		moveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, _ string) (*graph.Item, error) {
			assert.Fail(t, "a changed file must not be moved")
			return nil, errors.New("unexpected move")
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "b.txt",
		OldPath: "a.txt",
		ItemID:  "item-file",
		DriveID: driveID,
		View: &pathView{
			Path:   "a.txt",
			Remote: &remoteState{DriveID: driveID, ItemID: "item-file", ItemType: ItemTypeFile, ETag: `"etag-planned"`},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

// Validates: R-2.8.10, R-2.17.2
func TestExecutor_RemoteMove_ItemAlreadyAtDestinationSucceedsWithoutMoving(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		// A parent folder move already carried the item to the destination.
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{
				ID: "item-inner", DriveID: driveID, IsFolder: true, Name: "inner",
				ParentPath: "movedst", ParentPathKnown: true, ETag: `"etag-after-parent-move"`,
			}, nil
		},
		moveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, _ string) (*graph.Item, error) {
			assert.Fail(t, "an item already at its destination must not be moved again")
			return nil, errors.New("unexpected move")
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "movedst/inner",
		OldPath: "movesrc/inner",
		ItemID:  "item-inner",
		DriveID: driveID,
		View: &pathView{
			Path:   "movesrc/inner",
			Remote: &remoteState{DriveID: driveID, ItemID: "item-inner", ItemType: ItemTypeFolder, ETag: `"etag-planned"`},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "movedst/inner", o.Path)
	assert.Equal(t, `"etag-after-parent-move"`, o.ETag)
}

// Validates: R-2.17.9
//
// A local move that replaces its destination (OneDrive moved an item over a
// file it deleted) replaces only the version the plan saw; a destination that
// changed since planning is stale and stays untouched.
func TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		destination string
		wantReplace bool
	}{
		{name: "unchanged destination is replaced", destination: "bravo\n", wantReplace: true},
		{name: "changed destination is stale", destination: "edited after planning\n", wantReplace: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
			e := newExecution(cfg, emptyBaseline())
			writeExecTestFile(t, syncRoot, "a.txt", "alpha\n")
			writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
			plannedHash, err := e.hashFunc(filepath.Join(syncRoot, "b.txt"))
			require.NoError(t, err)
			info, err := os.Stat(filepath.Join(syncRoot, "b.txt"))
			require.NoError(t, err)
			if tc.destination != "bravo\n" {
				writeExecTestFile(t, syncRoot, "b.txt", tc.destination)
			}

			action := &Action{
				Type:               ActionLocalMove,
				Path:               "b.txt",
				OldPath:            "a.txt",
				ReplaceLocalTarget: true,
				View: &pathView{
					Path:     "b.txt",
					Local:    &localState{ItemType: ItemTypeFile, Hash: plannedHash, Size: info.Size()},
					Baseline: &BaselineEntry{Path: "a.txt", ItemType: ItemTypeFile},
				},
			}

			o := e.ExecuteMove(t.Context(), action)
			data, readErr := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
			require.NoError(t, readErr)
			if tc.wantReplace {
				requireOutcomeSuccess(t, &o)
				assert.Equal(t, "alpha\n", string(data))
				assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
				return
			}
			requireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, errActionPreconditionChanged)
			assert.Equal(t, tc.destination, string(data), "a changed destination is never replaced")
			assert.FileExists(t, filepath.Join(syncRoot, "a.txt"))
		})
	}
}

// Validates: R-2.17.10
//
// A directory's conflict copy is a rename of the directory to a free
// conflict name (D-059a). It used to be renamed onto a placeholder file and
// failed with ENOTDIR on every pass.
func TestExecutor_ConflictCopy_DirectorySourceIsRenamed(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, filepath.Join("thing", "child.txt"), "child")

	o := e.ExecuteConflictCopy(t.Context(), &Action{Type: ActionConflictCopy, Path: "thing", View: &pathView{Path: "thing"}})
	requireOutcomeSuccess(t, &o)

	assert.NoDirExists(t, filepath.Join(syncRoot, "thing"))
	assert.Contains(t, o.OldPath, "thing.conflict-")
	assert.FileExists(t, filepath.Join(syncRoot, o.OldPath, "child.txt"))
}
