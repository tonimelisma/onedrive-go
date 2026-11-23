package syncexec

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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	execHelloWorldContent      = "hello world"
	execHelloWorldQuickXorHash = "aCgDG9jwBhDc4Q1yawMZAAAAAAA="
)

// ---------------------------------------------------------------------------
// Mock types (prefixed to avoid collision with other test files)
// ---------------------------------------------------------------------------

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

func newTestExecutorConfig(t *testing.T, items *syncfixture.ExecutorMockItemClient, dl *syncfixture.ExecutorMockDownloader, ul *syncfixture.ExecutorMockUploader) (*ExecutorConfig, string) {
	t.Helper()

	return newTestExecutorConfigWithPathConvergence(t, items, dl, ul, nil)
}

func newTestExecutorConfigWithPathConvergence(
	t *testing.T,
	items *syncfixture.ExecutorMockItemClient,
	dl *syncfixture.ExecutorMockDownloader,
	ul *syncfixture.ExecutorMockUploader,
	pathConvergence driveops.PathConvergence,
) (*ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := mustNewExecutorConfig(t, &ExecutorInputs{
		Items: items, Downloads: dl, Uploads: ul, SyncTree: syncTree, DriveID: driveID, Logger: logger,
		PathConvergence: pathConvergence,
		Now:             func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) },
	})

	return cfg, syncRoot
}

// testMountRootID is the remote root item of a mount-root executor in tests.
const testMountRootID = "mount-root-id"

// mustNewExecutorConfig builds a configuration from in, filling what it
// leaves zero of the transfer manager (over its downloader and uploader) and
// the clock (the wall clock).
func mustNewExecutorConfig(t *testing.T, in *ExecutorInputs) *ExecutorConfig {
	t.Helper()

	if in.Transfers == nil {
		in.Transfers = driveops.NewTransferManager(in.Downloads, in.Uploads, nil, in.Logger)
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	cfg, err := NewExecutorConfig(in)
	require.NoError(t, err)

	return cfg
}

// setTestContentFilter replaces cfg's content filter as NewExecutorConfig
// installs one.
func setTestContentFilter(cfg *ExecutorConfig, filter syncdomain.ContentFilterConfig) {
	cfg.ignoreJunkFiles = filter.IgnoreJunkFiles
	cfg.contentFilter = syncdomain.NewContentFilter(syncdomain.CloneContentFilterConfig(filter))
}

func TestNewExecutorConfig_AllowsNilPathConvergence(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})

	require.Nil(t, cfg.PathConvergenceForTest())
}

// ---------------------------------------------------------------------------
// Folder create tests
// ---------------------------------------------------------------------------

func TestExecutor_CreateLocalFolder(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "docs/notes",
		CreateSide: syncdomain.CreateLocal,
		View: &syncdomain.PathView{
			Remote: &syncdomain.RemoteState{
				ItemID: "folder1",
				Mtime:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	info, err := os.Stat(filepath.Join(syncRoot, "docs", "notes"))
	require.NoError(t, err, "folder not created")
	require.True(t, info.IsDir(), "expected directory")
}

// A folder sync creates in the sync tree is asked for 0777 and gets the
// umask, like the files downloaded into it. It used to be 0755 whatever the
// umask, so under 002 the files were group-writable and their folder was not.
//
// Validates: R-5.10.2
func TestExecutor_CreatedFolderFollowsTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "docs/notes",
		CreateSide: syncdomain.CreateLocal,
		View:       &syncdomain.PathView{Remote: &syncdomain.RemoteState{ItemID: "folder1"}},
	})
	syncfixture.RequireOutcomeSuccess(t, &o)

	for _, rel := range []string{"docs", "docs/notes"} {
		info, err := os.Stat(filepath.Join(syncRoot, rel))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "%s is 0777 minus the umask", rel)
	}
}

func TestExecutor_CreateRemoteFolder(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			assert.Equal(t, "root", parentID, "unexpected parentID")
			assert.Equal(t, "photos", name, "unexpected name")

			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "photos",
		CreateSide: syncdomain.CreateRemote,
		View:       &syncdomain.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "new-folder-id", o.ItemID)
}

func TestExecutor_CreateRemoteFolder_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "shared",
		ItemID:   "shared-parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "photos",
		CreateSide: syncdomain.CreateRemote,
		View:       &syncdomain.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	require.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_PathConvergenceWarningIsNonFatal(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: driveops.ErrPathNotVisible}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "photos",
		CreateSide: syncdomain.CreateRemote,
		View:       &syncdomain.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_WaitsForParentVisibilityBeforeCreate(t *testing.T) {
	t.Parallel()

	pathConvergence := &executorPathConvergenceStub{}
	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "child", name)
			assert.Equal(t, []string{"parent"}, pathConvergence.waitCalls)
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "parent",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "parent/child",
		CreateSide: syncdomain.CreateRemote,
		View:       &syncdomain.PathView{Path: "parent/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"parent", "parent/child"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10
func TestExecutor_CreateRemoteFolder_MissingParentPreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "parent-id", itemID)
			return nil, graph.ErrNotFound
		},
		CreateFolderFn: func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
			require.FailNow(t, "folder create should not run after stale parent preflight")
			return nil, assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "parent",
		ItemID:   "parent-id",
		DriveID:  driveID,
		ItemType: syncdomain.ItemTypeFolder,
	}))

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "parent/child",
		CreateSide: syncdomain.CreateRemote,
		DriveID:    driveID,
		View:       &syncdomain.PathView{Path: "parent/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
}

func TestExecutor_CreateRemoteFolder_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	var capturedDriveID driveid.ID

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, driveID driveid.ID, parentID, _ string) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, sharedParent, parentID)
			return &graph.Item{ID: "new-folder-id", ETag: "etag1"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	cfg.remoteRootItemID = sharedParent
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "photos",
		CreateSide: syncdomain.CreateRemote,
		DriveID:    driveid.New("00000000000000ff"),
		View:       &syncdomain.PathView{Path: "photos"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
	assert.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}

func TestExecutor_CreateRemoteFolder_Error(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "restricted",
		CreateSide: syncdomain.CreateRemote,
		View:       &syncdomain.PathView{Path: "restricted"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability)
}

// ---------------------------------------------------------------------------
// Move tests
// ---------------------------------------------------------------------------

func TestExecutor_LocalMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "old-name.txt", "content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View:    &syncdomain.PathView{Path: "new-name.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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
	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	coexist := syncfixture.NamesCoexist(t, "edit.txt", "EDIT.txt")
	syncfixture.WriteExecTestFile(t, syncRoot, "edit.txt", "content")
	if coexist {
		require.NoError(t, os.Link(filepath.Join(syncRoot, "edit.txt"), filepath.Join(syncRoot, "EDIT.txt")))
	}

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "EDIT.txt",
		OldPath: "edit.txt",
		View:    &syncdomain.PathView{Path: "EDIT.txt"},
	}

	require.NoError(t, e.validateLocalMovePrecondition(t.Context(), action, action.OldPath, action.Path))
}

// Validates: R-2.12.4
func TestExecutor_LocalMove_SpellingOnlyRenameOntoDifferentFileIsStale(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.SkipUnlessNamesCoexist(t, "edit.txt", "EDIT.txt")

	syncfixture.WriteExecTestFile(t, syncRoot, "edit.txt", "content")
	syncfixture.WriteExecTestFile(t, syncRoot, "EDIT.txt", "other content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "EDIT.txt",
		OldPath: "edit.txt",
		View:    &syncdomain.PathView{Path: "EDIT.txt"},
	}

	require.ErrorIs(t, e.validateLocalMovePrecondition(t.Context(), action, action.OldPath, action.Path), syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.12.4
func TestExecutor_LocalMove_SpellingOnlyRenameLeavesNoTemporarySibling(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "stra\u00dfe.txt", "content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "STRASSE.txt",
		OldPath: "stra\u00dfe.txt",
		View:    &syncdomain.PathView{Path: "STRASSE.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "target.txt",
		OldPath: "nonexistent.txt",
		View:    &syncdomain.PathView{Path: "target.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "old-name.txt", "new content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
		View: &syncdomain.PathView{
			Path: "new-name.txt",
			Baseline: &syncdomain.BaselineEntry{
				Path:      "old-name.txt",
				ItemType:  syncdomain.ItemTypeFile,
				LocalHash: "hash-before-the-local-edit",
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "a.txt", "a")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "missing-parent/a.txt",
		OldPath: "a.txt",
		View: &syncdomain.PathView{
			Path:     "missing-parent/a.txt",
			Baseline: &syncdomain.BaselineEntry{Path: "a.txt", ItemType: syncdomain.ItemTypeFile},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.FileExists(t, filepath.Join(syncRoot, "a.txt"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "missing-parent"))
}

// Validates: R-2.8.10
func TestExecutor_LocalMove_FolderIdentityChangedReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	oldPath := filepath.Join(syncRoot, "old-folder")
	newPath := filepath.Join(syncRoot, "new-folder")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	currentIdentity, err := e.syncTree.IdentityNoFollow("old-folder")
	require.NoError(t, err)

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "new-folder",
		OldPath: "old-folder",
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{
				ItemType:         syncdomain.ItemTypeFolder,
				LocalDevice:      currentIdentity.Device,
				LocalInode:       currentIdentity.Inode ^ 1,
				LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)
	assert.DirExists(t, oldPath)
	assert.NoDirExists(t, newPath)
}

func TestExecutor_RemoteMove(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, "root", newParentID)
			assert.Equal(t, "renamed.txt", newName)

			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "renamed.txt", o.Path)
	assert.Equal(t, "original.txt", o.OldPath)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_StaleSourcePreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    "etag-current",
			}, nil
		},
		MoveItemFn: func(context.Context, driveid.ID, string, string, string) (*graph.Item, error) {
			require.FailNow(t, "remote move should not run after stale source preflight")
			return nil, assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "renamed.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     "etag-planned",
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_UsesConditionalETagFromPreflight(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		MoveItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, GraphRootID, newParentID)
			assert.Equal(t, "renamed.txt", newName)
			assert.Equal(t, `"etag-current"`, ifMatch)
			return &graph.Item{ID: "item1", ETag: `"etag-next"`}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "renamed.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, `"etag-next"`, o.ETag)
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_ConditionalMismatchReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		MoveItemIfMatchFn: func(context.Context, driveid.ID, string, string, string, string) (*graph.Item, error) {
			return nil, graph.ErrPreconditionFailed
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "renamed.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability)
}

func TestExecutor_RemoteMove_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"renamed.txt"}, pathConvergence.waitCalls)
}

func TestExecutor_RemoteMove_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag2"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, _ := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, pathConvergence)
	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New("00000000000000ff"),
		View:    &syncdomain.PathView{Path: "renamed.txt"},
	}

	cfg.remoteRootItemID = testMountRootID
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"renamed.txt"}, pathConvergence.waitCalls)
}

// ---------------------------------------------------------------------------
// Download tests
// ---------------------------------------------------------------------------

// Validates: R-5.1
func TestExecutor_Download_Success(t *testing.T) {
	t.Parallel()

	execFileContent := execHelloWorldContent

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte(execFileContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "greetings.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Remote: &syncdomain.RemoteState{
				ItemID: "item1",
				ETag:   "etag1",
				Mtime:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:              "item1",
				DriveID:         driveID,
				Name:            "shortcut-sentinel.txt",
				ParentPath:      "Writable Shared Folder",
				ParentPathKnown: true,
				Size:            int64(len(execHelloWorldContent)),
			}, nil
		},
	}
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte(execHelloWorldContent))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, dl, &syncfixture.ExecutorMockUploader{})
	cfg.remoteRootItemID = testMountRootID
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "shortcut-sentinel.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "shortcut-sentinel.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				Size:     int64(len(execHelloWorldContent)),
			},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.FileExists(t, filepath.Join(syncRoot, "shortcut-sentinel.txt"))
}

func TestExecutor_Download_APIError(t *testing.T) {
	t.Parallel()

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "exec-forbidden.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, o.FailureCapability)
}

// Validates: R-5.10.3
func TestExecutor_Download_MissingParentIsStaleAndNotCreated(t *testing.T) {
	t.Parallel()

	var downloads int
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloads++
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	// The planned parent "A" was renamed away locally after planning.
	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "A/exec-dl.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)

	// It used to re-create an empty A/ holding the download, splitting the
	// user's renamed folder in two.
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Zero(t, downloads)
	assert.NoDirExists(t, filepath.Join(syncRoot, "A"))
}

// Validates: R-5.10.3
func TestExecutor_Download_ParentSwappedForSymlinkAfterCheckStaysInRoot(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	moved := filepath.Join(outside, "moved")
	var syncRoot string
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Mid-download, after every check: the parent folder moves out
			// of the sync root and a symlink to it takes its place.
			parent := filepath.Join(syncRoot, "A")
			require.NoError(t, os.Rename(parent, moved))
			require.NoError(t, os.Symlink(moved, parent))

			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, root := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	syncRoot = root
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "A"), 0o700))
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteDownload(t.Context(), &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "A/escape.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Mtime: 1}},
	})

	// Absolute paths followed the swapped symlink, and the download was
	// renamed into place outside the sync root.
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.NoFileExists(t, filepath.Join(moved, "escape.txt"), "nothing may be published outside the sync root")
}

func TestExecutor_Download_ZeroByte(t *testing.T) {
	t.Parallel()

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "exec-empty.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	info, err := os.Stat(filepath.Join(syncRoot, "exec-empty.txt"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "expected zero-byte file")
}

// Validates: R-2.8.10
func TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
			}, nil
		},
	}
	var syncRoot string
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote content"))
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "appeared.txt"), []byte("local drift"), 0o600))
			return int64(n), err
		},
	}

	cfg, root := newTestExecutorConfig(t, items, dl, &syncfixture.ExecutorMockUploader{})
	syncRoot = root
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "appeared.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path:   "appeared.txt",
			Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item1", ItemType: syncdomain.ItemTypeFile},
		},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)

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

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	correctHash := execHelloWorldQuickXorHash // QuickXorHash of hello world.
	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "hash-retry.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, 3, callCount, "expected 3 download calls")
	assert.Equal(t, correctHash, o.LocalHash)
	assert.Equal(t, correctHash, o.RemoteHash)

	// File should contain correct content.
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "hash-retry.txt"))
	require.NoError(t, err)
	assert.Equal(t, execHelloWorldContent, string(data))
}

// Validates: R-5.8.2
func TestExecutor_Download_DeterministicTransformRecordsRemoteHash(t *testing.T) {
	t.Parallel()

	served := []byte("served bytes")
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(served)
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteDownload(t.Context(), &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "transformed.docx",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Hash: "reported-hash", Size: int64(len(served)), Mtime: 1}},
	})
	syncfixture.RequireOutcomeSuccess(t, &o)

	// Each side records what it has: OneDrive's hash as the remote half,
	// the bytes' hash as the local half, so neither looks changed next pass.
	assert.Equal(t, "reported-hash", o.RemoteHash)
	assert.Equal(t, syncfixture.HashContentQuickXor(t, string(served)), o.LocalHash)
}

func TestExecutor_Download_HashMatch_NoRetry(t *testing.T) {
	t.Parallel()

	callCount := 0
	content := execHelloWorldContent

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			n, err := w.Write([]byte(content))
			return int64(n), err
		},
	}

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	correctHash := execHelloWorldQuickXorHash
	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "hash-ok.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Hash: correctHash, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, 1, callCount, "expected 1 download call")
	assert.Equal(t, correctHash, o.LocalHash)
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

// Validates: R-5.1
func TestExecutor_Upload_SimpleSuccess(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "root", parentID)
			assert.Equal(t, "exec-small.txt", name)

			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "uploaded1", o.ItemID)
	assert.Equal(t, "root", o.ParentID)
}

func TestExecutor_Upload_APIError(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Equal(t, action.Path, o.FailurePath)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability)
}

func TestExecutor_Upload_UsesPathConvergence(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent path goes through the convergence boundary")
}

func TestExecutor_Upload_CreateByParentWaitsForParentVisibilityBeforeUpload(t *testing.T) {
	t.Parallel()

	pathConvergence := &executorPathConvergenceStub{}
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "exec-small.txt", name)
			assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	require.Equal(t, []string{"folder"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10, R-2.16.2
func TestExecutor_Upload_CreateByParentWaitsBeforeParentPrecondition(t *testing.T) {
	t.Parallel()

	parentWaited := false
	getItemCalls := 0
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
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
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.True(t, parentWaited, "parent path convergence should run before parent preflight and upload")
			assert.Equal(t, "parent-id", parentID)
			assert.Equal(t, "exec-small.txt", name)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, 1, getItemCalls)
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent is probed; the uploaded file is not")
}

// Validates: R-2.8.7, R-2.8.10
func TestExecutor_Upload_CreateByParentMissingAfterVisibilityWaitReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			require.Equal(t, driveid.New(synctest.TestDriveID), driveID)
			require.Equal(t, "parent-id", itemID)
			return nil, graph.ErrNotFound
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: driveops.ErrPathNotVisible}
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not start when parent preflight proves stale work")
			return nil, assert.AnError
		},
	}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, o.FailureCapability)
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10
//
// An upload's response carries its item, and sync reads no file by path
// afterwards, so the uploaded path is not probed for visibility: a probe
// that fails must not be able to touch the outcome, and the request is not
// spent.
func TestExecutor_Upload_DoesNotProbeItsOwnPathAfterTheUpload(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{waitErr: errors.New("metadata probe failed")}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "exec-small.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Empty(t, pathConvergence.waitCalls, "a top-level upload makes no visibility probe at all")
}

func TestExecutor_Upload_CrossDriveParentUsesTargetScopedPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	var capturedDriveID driveid.ID

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, driveID driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			assert.Equal(t, "parent-id", parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	cfg.remoteRootItemID = sharedParent
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New("00000000000000ff"),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, driveid.New("00000000000000ff"), capturedDriveID)
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent is probed by its mount-relative path")
}

func TestExecutor_Upload_MountRootUsesMountLocalPathConvergence(t *testing.T) {
	t.Parallel()

	const sharedParent = "shared-parent-id"

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "parent-id", parentID)
			return &graph.Item{ID: "uploaded1", ETag: "etag1", QuickXorHash: "abc"}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)
	cfg.remoteRootItemID = sharedParent
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New("00000000000000ff"),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-small.txt", "hello")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-small.txt",
		DriveID: driveid.New("00000000000000ff"),
		View:    &syncdomain.PathView{Path: "folder/exec-small.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent is probed by its mount-local path")
}

func TestExecutor_Upload_ParentFromBaseline(t *testing.T) {
	t.Parallel()

	var capturedParentID string

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedParentID = parentID
			return &graph.Item{ID: "uploaded3", ETag: "e3"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "exec-existing-dir",
		ItemID:   "baseline-folder-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-existing-dir/exec-doc.txt", "content")

	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "exec-existing-dir/exec-doc.txt",
		View: &syncdomain.PathView{Path: "exec-existing-dir/exec-doc.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "baseline-folder-id", capturedParentID)
}

func TestExecutor_Upload_KnownItemUsesItemOverwrite(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "parent-path upload should not be used when item ID is known")
			return nil, errors.New("unexpected Upload call")
		},
		UploadToItemFn: func(_ context.Context, _ driveid.ID, itemID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "known-item-id", itemID)

			return &graph.Item{
				ID:           "known-item-id",
				ParentID:     "parent-from-item",
				ETag:         "etag-overwrite",
				QuickXorHash: "overwrite-hash",
			}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "known.txt",
		ItemID:   "known-item-id",
		ParentID: "baseline-parent",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFile,
	}))

	syncfixture.WriteExecTestFile(t, syncRoot, "known.txt", "known content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "known.txt",
		ItemID:  "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "known.txt",
			Baseline: &syncdomain.BaselineEntry{
				ParentID: "baseline-parent",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, "known-item-id", o.ItemID)
	assert.Equal(t, "parent-from-item", o.ParentID)
}

func TestExecutor_Upload_ZeroDriveIDIsFilled(t *testing.T) {
	t.Parallel()

	var capturedDriveID driveid.ID

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, driveID driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedDriveID = driveID
			return &graph.Item{ID: "up1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-new-file.txt", "data")

	// New local item has zero DriveID until execution binds the mounted drive.
	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "exec-new-file.txt",
		View: &syncdomain.PathView{Path: "exec-new-file.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	// Executor should have filled driveID from its own context.
	assert.Equal(t, driveid.New(synctest.TestDriveID), capturedDriveID)
}

// Validates: R-5.1
func TestExecutor_Upload_LargeFileSuccess(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _ string, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "chunked1", ETag: "ce1", QuickXorHash: "hash1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	// Create a file > 4 MiB to exercise Upload for large files.
	bigContent := strings.Repeat("x", 5*1024*1024) // 5 MiB
	syncfixture.WriteExecTestFile(t, syncRoot, "exec-big-file.bin", bigContent)

	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "exec-big-file.bin",
		View: &syncdomain.PathView{Path: "exec-big-file.bin"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "chunked1", o.ItemID)
}

// Validates: R-2.8.10
func TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not start after local source precondition changes")
			return nil, assert.AnError
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "changed-before-upload.txt", "local drift")

	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "changed-before-upload.txt",
		View: &syncdomain.PathView{
			Path: "changed-before-upload.txt",
			Local: &syncdomain.LocalState{
				ItemType: syncdomain.ItemTypeFile,
				Hash:     execHelloWorldQuickXorHash,
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalRead, o.FailureCapability)
}

// Validates: R-2.4.6, R-2.8.10
func TestExecutor_Upload_SymlinkAliasSourceUsesFollowedTargetPrecondition(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "alias.txt", name)
			assert.Equal(t, int64(len(execHelloWorldContent)), size)
			buf := make([]byte, size)
			_, err := content.ReadAt(buf, 0)
			require.NoError(t, err)
			assert.Equal(t, execHelloWorldContent, string(buf))
			return &graph.Item{ID: "uploaded-alias", ETag: "etag1", QuickXorHash: execHelloWorldQuickXorHash}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	targetPath := syncfixture.WriteExecTestFile(t, syncRoot, "target.txt", execHelloWorldContent)
	aliasPath := filepath.Join(syncRoot, "alias.txt")
	if err := os.Symlink(targetPath, aliasPath); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}
	info, err := os.Stat(aliasPath)
	require.NoError(t, err)
	identity, identityOK := synctree.IdentityFromFileInfo(info)
	require.True(t, identityOK)

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "alias.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "alias.txt",
			Local: &syncdomain.LocalState{
				ItemType:         syncdomain.ItemTypeFile,
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
	syncfixture.RequireOutcomeSuccess(t, &o)
}

// ---------------------------------------------------------------------------
// Local delete tests
// ---------------------------------------------------------------------------

// Validates: R-6.2.4, R-6.4.1
func TestExecutor_LocalDelete_HashMatch(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	absPath := syncfixture.WriteExecTestFile(t, syncRoot, "exec-delete-me.txt", "content")

	hash, err := driveops.ComputeQuickXorHashContext(t.Context(), absPath)
	require.NoError(t, err)

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-delete-me.txt",
		ItemID: "item1",
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{LocalHash: hash},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "file should have been deleted")
}

// Validates: R-6.2.4, R-6.4.1, R-2.8.10
func TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	uploadCalled := false
	uploader := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			assert.Equal(t, GraphRootID, parentID)
			assert.Equal(t, "exec-modified.txt", name)
			return &graph.Item{ID: "uploaded-item", ETag: "etag1", QuickXorHash: "remote-hash"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, uploader)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-modified.txt", "new content")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	require.False(t, o.Success)
	assert.Equal(t, syncdomain.ActionLocalDelete, o.Action)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)

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

// Validates: R-6.2.4, R-6.4.1
func TestExecutor_LocalDelete_HashMismatch_DoesNotCreateConflictCopy(t *testing.T) {
	t.Parallel()

	uploadCalled := false
	uploader := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			assert.Equal(t, GraphRootID, parentID)
			assert.Equal(t, "exec-modified.txt", name)
			return &graph.Item{ID: "uploaded-item", ETag: "etag1", QuickXorHash: "remote-hash"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, uploader)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-modified.txt", "new content")
	syncfixture.WriteExecTestFile(t, syncRoot, "exec-modified.conflict-20260115-120000.txt", "existing conflict")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-modified.txt",
		ItemID: "item1",
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{
				LocalHash:  "old-hash-that-wont-match",
				RemoteHash: "baseline-remote-hash",
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	require.False(t, o.Success)
	assert.Equal(t, syncdomain.ActionLocalDelete, o.Action)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)

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

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-already-gone.txt",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)
}

func TestExecutor_LocalDelete_FolderEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "exec-empty-dir"), 0o700))

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-empty-dir",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(filepath.Join(syncRoot, "exec-empty-dir"))
	assert.True(t, os.IsNotExist(statErr), "directory should have been removed")
}

// Validates: R-2.8.10
func TestExecutor_LocalDelete_FolderIdentityChangedReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	dirPath := filepath.Join(syncRoot, "exec-folder-recreated")
	require.NoError(t, os.MkdirAll(dirPath, 0o700))
	currentIdentity, err := e.syncTree.IdentityNoFollow("exec-folder-recreated")
	require.NoError(t, err)

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-folder-recreated",
		ItemID: "item1",
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{
				ItemType:         syncdomain.ItemTypeFolder,
				LocalDevice:      currentIdentity.Device,
				LocalInode:       currentIdentity.Inode ^ 1,
				LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)
	assert.DirExists(t, dirPath)
}

func TestExecutor_LocalDelete_FolderNotEmpty(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-non-empty-dir/child.txt", "data")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "exec-non-empty-dir",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	outside := t.TempDir()
	outsideFile := syncfixture.WriteExecTestFile(t, outside, "target/file.txt", "outside data")
	aliasPath := filepath.Join(syncRoot, "alias")
	if err := os.Symlink(filepath.Join(outside, "target"), aliasPath); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "alias/file.txt",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := e.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "won't delete alias/file.txt: alias is a symbolic link")
	assert.FileExists(t, outsideFile)
	assert.FileExists(t, filepath.Join(syncRoot, "alias", "file.txt"))
}

// ---------------------------------------------------------------------------
// Remote delete tests
// ---------------------------------------------------------------------------

func TestExecutor_RemoteDelete_Success(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		DeleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			assert.Equal(t, "item1", itemID, "unexpected itemID")

			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
}

// Validates: R-2.8.10
func TestExecutor_RemoteDelete_UsesConditionalETagFromPreflight(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		DeleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, ifMatch string) error {
			assert.Equal(t, "item1", itemID)
			assert.Equal(t, `"etag-current"`, ifMatch)
			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "exec-remote-file.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
}

// Validates: R-2.8.10
func TestExecutor_RemoteDelete_ConditionalMismatchReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveID,
				ETag:    `"etag-current"`,
			}, nil
		},
		DeleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			return graph.ErrPreconditionFailed
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "exec-remote-file.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_RemoteDelete_WrongDrivePreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	plannedDriveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "item1", itemID)
			return &graph.Item{
				ID:      "item1",
				DriveID: driveid.New("other-drive"),
				ETag:    `"etag-current"`,
			}, nil
		},
		DeleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			require.FailNow(t, "remote delete should not run after wrong-drive preflight")
			return assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "exec-remote-file.txt",
		ItemID:  "item1",
		DriveID: plannedDriveID,
		View: &syncdomain.PathView{
			Path: "exec-remote-file.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  plannedDriveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, o.FailureCapability)
}

// Validates: R-2.8.10, R-6.2.4
func TestExecutor_RemoteDelete_StalePathPreflightReturnsStalePrecondition(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
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
		DeleteItemIfMatchFn: func(context.Context, driveid.ID, string, string) error {
			require.FailNow(t, "remote delete should not run after stale-path preflight")
			return assert.AnError
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "Docs/file.txt",
		ItemID:  "item1",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "Docs/file.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "item1",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     `"etag-current"`,
			},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, o.FailureCapability)
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
		wantFailureCapability syncdomain.PermissionCapability
	}{
		{
			name:                  "404AfterPreflightIsSuperseded",
			path:                  "exec-already-deleted.txt",
			itemID:                "item2",
			deleteErr:             graph.ErrNotFound,
			wantErrIs:             syncdomain.ErrActionPreconditionChanged,
			wantFailurePath:       "exec-already-deleted.txt",
			wantFailureCapability: syncdomain.PermissionCapabilityRemoteWrite,
		},
		{
			name:                  "403Skip",
			path:                  "exec-forbidden-del.txt",
			itemID:                "item3",
			deleteErr:             graph.ErrForbidden,
			wantErrIs:             graph.ErrForbidden,
			wantFailurePath:       "exec-forbidden-del.txt",
			wantFailureCapability: syncdomain.PermissionCapabilityRemoteWrite,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			items := &syncfixture.ExecutorMockItemClient{
				DeleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
					return tt.deleteErr
				},
			}

			cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			e := NewExecution(cfg, syncfixture.EmptyBaseline())

			action := &syncdomain.Action{
				Type:    syncdomain.ActionRemoteDelete,
				Path:    tt.path,
				ItemID:  tt.itemID,
				DriveID: driveid.New(synctest.TestDriveID),
				View:    &syncdomain.PathView{},
			}

			o := e.ExecuteRemoteDelete(t.Context(), action)
			if tt.wantOK {
				syncfixture.RequireOutcomeSuccess(t, &o)
				return
			}

			syncfixture.RequireOutcomeFailure(t, &o)
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

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")

	copyAction := &syncdomain.Action{
		Type:    syncdomain.ActionConflictCopy,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Local: &syncdomain.LocalState{
				ItemType: syncdomain.ItemTypeFile,
			},
			Remote: &syncdomain.RemoteState{
				ItemID: "item1",
				ETag:   "etag1",
			},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	syncfixture.RequireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = syncdomain.ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	syncfixture.RequireOutcomeSuccess(t, &o)

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
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			downloadCalled = true
			return 0, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-conflict-reappears.txt", "original local")

	copyAction := &syncdomain.Action{
		Type:    syncdomain.ActionConflictCopy,
		Path:    "exec-conflict-reappears.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Local:  &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile},
			Remote: &syncdomain.RemoteState{ItemID: "item1"},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	syncfixture.RequireOutcomeSuccess(t, &copyOutcome)

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-conflict-reappears.txt", "fresh local")

	downloadAction := *copyAction
	downloadAction.Type = syncdomain.ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityLocalWrite, o.FailureCapability)
	assert.False(t, downloadCalled)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-conflict-reappears.txt"))
	require.NoError(t, err)
	assert.Equal(t, "fresh local", string(data))
}

// Validates: R-2.3.1
func TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix(t *testing.T) {
	t.Parallel()

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote version"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-conflict.txt", "local version")
	syncfixture.WriteExecTestFile(t, syncRoot, "exec-conflict.conflict-20260115-120000.txt", "existing conflict")

	copyAction := &syncdomain.Action{
		Type:    syncdomain.ActionConflictCopy,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Remote: &syncdomain.RemoteState{
				ItemID: "item1",
				ETag:   "etag1",
			},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	syncfixture.RequireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = syncdomain.ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:   "new-item",
				Name: name,
				ETag: "etag-new",
			}, nil
		},
		UploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadToItemCalled = true
			return nil, errors.New("unexpected UploadToItem call")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "parent-folder",
		ItemType: syncdomain.ItemTypeFolder,
	})
	e := NewExecution(cfg, baseline)

	// Local file exists with modified content (edit-delete: local modified,
	// remote deleted).
	syncfixture.WriteExecTestFile(t, syncRoot, "folder/exec-ed-file.txt", "locally modified data")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/exec-ed-file.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "folder/exec-ed-file.txt",
			Remote: &syncdomain.RemoteState{
				ItemID:    "deleted-item",
				DriveID:   driveid.New(synctest.TestDriveID),
				ItemType:  syncdomain.ItemTypeFile,
				IsDeleted: true,
			},
			Baseline: &syncdomain.BaselineEntry{
				Path:     "folder/exec-ed-file.txt",
				DriveID:  driveid.New(synctest.TestDriveID),
				ItemID:   "deleted-item",
				ParentID: "parent-folder",
				ItemType: syncdomain.ItemTypeFile,
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.True(t, uploadCalled, "expected edit-delete handling to recreate the remote file")
	assert.False(t, uploadToItemCalled, "edit-delete handling should not overwrite a deleted item ID")
	assert.Equal(t, syncdomain.ActionUpload, o.Action)
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

	action := &syncdomain.Action{
		Type:    syncdomain.ActionBaselineUpdate,
		Path:    "exec-converged.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Remote: &syncdomain.RemoteState{
				ItemID:   "item1",
				Hash:     "hash1",
				Size:     1024,
				ETag:     "etag1",
				ItemType: syncdomain.ItemTypeFile,
			},
			Local: &syncdomain.LocalState{
				Hash:  "hash1",
				Mtime: 1234567890,
			},
		},
	}

	mutation, err := syncstore.PublicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
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

	action := &syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "exec-ghost.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
	}

	mutation, err := syncstore.PublicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, syncdomain.ActionCleanup, mutation.Action)
	assert.Equal(t, "exec-ghost.txt", mutation.Path)
}

// ---------------------------------------------------------------------------
// Parent ID resolution tests
// ---------------------------------------------------------------------------

func TestExecutor_ResolveParentID_Baseline(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "exec-existing-folder",
		ItemID:   "folder-id-from-baseline",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
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

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	cfg.remoteRootItemID = testMountRootID
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	id, err := e.ResolveParentID("exec-file.txt")
	require.NoError(t, err)
	assert.Equal(t, testMountRootID, id)
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

			stem, ext := syncdomain.ConflictStemExt(tt.input)
			assert.Equal(t, tt.wantStem, stem, "stem mismatch for %q", tt.input)
			assert.Equal(t, tt.wantExt, ext, "ext mismatch for %q", tt.input)
		})
	}
}

// Fix 9: Test concrete conflict actions leave the preserved local copy in place
// when the dependent download fails.
func TestExecutor_ConflictDownloadFails_LeavesConflictCopy(t *testing.T) {
	t.Parallel()

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrForbidden
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	originalContent := "precious local data"
	syncfixture.WriteExecTestFile(t, syncRoot, "exec-restore.txt", originalContent)

	copyAction := &syncdomain.Action{
		Type:    syncdomain.ActionConflictCopy,
		Path:    "exec-restore.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Local:  &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile},
			Remote: &syncdomain.RemoteState{ItemID: "item1"},
		},
	}

	copyOutcome := e.ExecuteConflictCopy(t.Context(), copyAction)
	syncfixture.RequireOutcomeSuccess(t, &copyOutcome)

	downloadAction := *copyAction
	downloadAction.Type = syncdomain.ActionDownload
	downloadAction.RequireMissingLocalTarget = true
	o := e.ExecuteDownload(t.Context(), &downloadAction)
	syncfixture.RequireOutcomeFailure(t, &o)

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

	items := &syncfixture.ExecutorMockItemClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return nil, graph.ErrForbidden
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "renamed.txt",
		OldPath: "original.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "renamed.txt"},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)

	require.ErrorIs(t, o.Error, graph.ErrForbidden)
	assert.Equal(t, action.OldPath, o.FailurePath)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability)
}

func TestInferFailureCapabilityFromError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		err              error
		localCapability  syncdomain.PermissionCapability
		remoteCapability syncdomain.PermissionCapability
		want             syncdomain.PermissionCapability
	}{
		{
			name:             "wrapped local permission",
			err:              fmt.Errorf("opening local file: %w", os.ErrPermission),
			localCapability:  syncdomain.PermissionCapabilityLocalRead,
			remoteCapability: syncdomain.PermissionCapabilityRemoteWrite,
			want:             syncdomain.PermissionCapabilityLocalRead,
		},
		{
			name:             "wrapped remote forbidden",
			err:              fmt.Errorf("uploading remote file: %w", graph.ErrForbidden),
			localCapability:  syncdomain.PermissionCapabilityLocalRead,
			remoteCapability: syncdomain.PermissionCapabilityRemoteWrite,
			want:             syncdomain.PermissionCapabilityRemoteWrite,
		},
		{
			name:             "non permission error",
			err:              errors.New("something else"),
			localCapability:  syncdomain.PermissionCapabilityLocalRead,
			remoteCapability: syncdomain.PermissionCapabilityRemoteWrite,
			want:             syncdomain.PermissionCapabilityUnknown,
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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	syncfixture.WriteExecTestFile(t, syncRoot, "exec-src.txt", "content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "exec-dst.txt",
		OldPath: "exec-src.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "exec-dst.txt",
			Remote: &syncdomain.RemoteState{
				Hash:     "remotehash",
				Size:     42,
				ETag:     "etag-move",
				ItemType: syncdomain.ItemTypeFile,
			},
			// What sat at the destination before the move (here a file the
			// move replaces) is not the moved item's local truth.
			Local: &syncdomain.LocalState{
				Hash:  "replaced-hash",
				Mtime: 1,
			},
			Baseline: &syncdomain.BaselineEntry{
				ItemType:   syncdomain.ItemTypeFile,
				LocalHash:  "localhash",
				LocalMtime: 9876543210,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, "remotehash", o.RemoteHash)
	assert.Equal(t, int64(42), o.RemoteSize)
	assert.True(t, o.RemoteSizeKnown)
	assert.Equal(t, "etag-move", o.ETag)
	assert.Equal(t, "localhash", o.LocalHash, "the rename preserves the source's synced content")
	assert.Equal(t, int64(9876543210), o.LocalMtime)
	assert.Equal(t, syncdomain.ItemTypeFile, o.ItemType)
}

// A move takes the baseline's remote modification time only when its view
// did not observe the remote side. A remote item observed with no time is
// recorded with none: the planner compares the baseline's remote time with
// the remote row's, and a baseline time put over an observed "no time" is a
// metadata difference and one more baseline update on the next pass.
//
// Validates: R-2.2.3
func TestExecutor_MoveOutcome_TakesTheBaselineRemoteTimeOnlyForAnUnobservedRemoteSide(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	baseline := &syncdomain.BaselineEntry{
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "hash",
		RemoteHash:      "hash",
		RemoteSize:      42,
		RemoteSizeKnown: true,
		RemoteMtime:     9876543210,
	}
	move := func(remote *syncdomain.RemoteState) syncdomain.ActionOutcome {
		return e.moveOutcome(&syncdomain.Action{
			Type:    syncdomain.ActionLocalMove,
			Path:    "exec-dst.txt",
			OldPath: "exec-src.txt",
			ItemID:  "item1",
			DriveID: driveid.New(synctest.TestDriveID),
			View:    &syncdomain.PathView{Path: "exec-dst.txt", Remote: remote, Baseline: baseline},
		})
	}

	observedWithNoTime := move(&syncdomain.RemoteState{Hash: "hash", Size: 42, ItemType: syncdomain.ItemTypeFile})
	assert.Zero(t, observedWithNoTime.RemoteMtime, "the remote item was observed, and it carries no time")

	observedWithATime := move(&syncdomain.RemoteState{Hash: "hash", Size: 42, Mtime: 5, ItemType: syncdomain.ItemTypeFile})
	assert.Equal(t, int64(5), observedWithATime.RemoteMtime)

	unobserved := move(nil)
	assert.Equal(t, baseline.RemoteMtime, unobserved.RemoteMtime, "the remote side was not observed")
}

// Fix 12: Test large-file upload delegates to Uploader with correct size.
func TestExecutor_Upload_LargeFileSizePassedToUploader(t *testing.T) {
	t.Parallel()

	var capturedSize int64

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedSize = size
			return &graph.Item{ID: "multi-chunk1", ETag: "mc1", QuickXorHash: "h1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	// 25 MiB file — Uploader receives the exact size.
	expectedSize := int64(25 * 1024 * 1024)
	bigContent := strings.Repeat("x", int(expectedSize))
	syncfixture.WriteExecTestFile(t, syncRoot, "exec-multi-chunk.bin", bigContent)

	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "exec-multi-chunk.bin",
		View: &syncdomain.PathView{Path: "exec-multi-chunk.bin"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, expectedSize, capturedSize)
}

// ---------------------------------------------------------------------------
// ItemType propagation tests (Fixes 3, 4, 5)
// ---------------------------------------------------------------------------

func TestExecutor_DeleteOutcome_FolderType(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{
		DeleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error { return nil },
		ListChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
			return nil, nil
		},
	}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "exec-folder-del",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder},
		},
	}

	o := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.Equal(t, syncdomain.ItemTypeFolder, o.ItemType)
}

func TestPublicationMutation_Cleanup_FolderType(t *testing.T) {
	t.Parallel()

	action := &syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "exec-cleanup-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder},
		},
	}

	mutation, err := syncstore.PublicationMutationFromAction(action, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)

	assert.Equal(t, syncdomain.ItemTypeFolder, mutation.ItemType)
}

// A baseline update's type is read from the side observed: a synced folder
// the planner sees locally (a folder carries no hash) and not remotely stays
// a folder, and one both sides replaced with the same file is that file,
// whatever the baseline recorded.
//
// Validates: R-2.17.4
func TestPublicationMutation_SyncedUpdate_TypeFromObservedSide(t *testing.T) {
	t.Parallel()

	folderBaseline := &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}
	localOnly := &syncdomain.Action{
		Type:    syncdomain.ActionBaselineUpdate,
		Path:    "exec-synced-folder",
		ItemID:  "folder1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Baseline: folderBaseline,
			Local:    &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder, Mtime: 123},
		},
	}

	mutation, err := syncstore.PublicationMutationFromAction(localOnly, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)
	assert.Equal(t, syncdomain.ItemTypeFolder, mutation.ItemType)

	replacedByFile := &syncdomain.Action{
		Type:    syncdomain.ActionBaselineUpdate,
		Path:    "exec-synced-folder",
		ItemID:  "file1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Baseline: folderBaseline,
			Remote:   &syncdomain.RemoteState{ItemID: "file1", ItemType: syncdomain.ItemTypeFile, Hash: "h"},
			Local:    &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "h"},
		},
	}

	mutation, err = syncstore.PublicationMutationFromAction(replacedByFile, driveid.New(synctest.TestDriveID))
	require.NoError(t, err)
	require.NotNil(t, mutation)
	assert.Equal(t, syncdomain.ItemTypeFile, mutation.ItemType)
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

	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Write some bytes first (partial content written to disk).
			n, writeErr := w.Write([]byte("partial data"))
			if writeErr != nil {
				return int64(n), writeErr
			}

			// Fail mid-stream — simulates network error after partial write.
			return int64(n), errors.New("connection reset after partial write")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "partial-cleanup.txt",
		ItemID:  "item-partial",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{}},
	}

	o := e.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)

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

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, mtime time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedMtime = mtime

			return &graph.Item{ID: "up-mtime", ETag: "e1"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	// Write a file and set a specific mtime.
	syncfixture.WriteExecTestFile(t, syncRoot, "mtime-test.txt", "mtime content")

	targetMtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	absPath := filepath.Join(syncRoot, "mtime-test.txt")

	require.NoError(t, os.Chtimes(absPath, targetMtime, targetMtime))

	action := &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "mtime-test.txt",
		View: &syncdomain.PathView{Path: "mtime-test.txt"},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

	// Verify the uploader received the file's mtime.
	assert.True(t, capturedMtime.Equal(targetMtime), "uploader received mtime %v, want %v", capturedMtime, targetMtime)

	// Verify the outcome also records the mtime.
	assert.Equal(t, targetMtime.UnixNano(), o.LocalMtime)
}

// Validates: R-6.4.8
func TestCreateLocalFolder_TraversalBlocked(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "../escape",
		CreateSide: syncdomain.CreateLocal,
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.ErrorIs(t, o.Error, syncdomain.ErrPathEscapesSyncRoot)
}

// ---------------------------------------------------------------------------
// Upload freshness check tests
// ---------------------------------------------------------------------------

func TestExecutor_Upload_ETagMismatch(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			// Remote has a different eTag — someone else edited the file.
			return &graph.Item{ID: "item1", ETag: "etag-remote-changed"}, nil
		},
	}

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not be called when eTag mismatches")
			return nil, errors.New("unexpected upload call")
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, ul)

	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "conflict.txt", "local content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "conflict.txt",
			Baseline: &syncdomain.BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Contains(t, o.Error.Error(), "upload overwrite remote item item1 changed since planning")
}

func TestExecutor_Upload_AlwaysChecksFreshnessWhenBaselineETagKnown(t *testing.T) {
	t.Parallel()

	getItemCalls := 0
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: "item1", ETag: "etag-baseline"}, nil
		},
	}

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item1", ETag: "etag1", QuickXorHash: "qxh"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, ul)

	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "normal.txt", "content")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "normal.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "normal.txt",
			Baseline: &syncdomain.BaselineEntry{
				ETag: "etag-baseline",
			},
		},
	}

	o := e.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, 1, getItemCalls, "expected one freshness probe before upload")
}

// moveTestFileBaseline builds the source baseline a real sync would have
// recorded for relPath, so local move preconditions pass against it.
func moveTestFileBaseline(t *testing.T, e *Executor, syncRoot, relPath string) *syncdomain.BaselineEntry {
	t.Helper()

	info, err := os.Stat(filepath.Join(syncRoot, relPath))
	require.NoError(t, err)
	hash, err := e.hashFile(t.Context(), filepath.Join(syncRoot, relPath))
	require.NoError(t, err)
	identity, err := e.syncTree.IdentityNoFollow(relPath)
	require.NoError(t, err)

	return &syncdomain.BaselineEntry{
		Path:             relPath,
		ItemID:           "item-moved",
		ItemType:         syncdomain.ItemTypeFile,
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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "renamed.txt", "hi")
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "sub"), 0o750))
	baseline := moveTestFileBaseline(t, e, syncRoot, "renamed.txt")

	// A server-side rename with unchanged content: only path and eTag move.
	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "sub/renamed-remotely.txt",
		OldPath: "renamed.txt",
		ItemID:  "item-moved",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "sub/renamed-remotely.txt",
			Remote: &syncdomain.RemoteState{
				ItemID: "item-moved", ItemType: syncdomain.ItemTypeFile,
				Hash: "remote-hash", Size: baseline.RemoteSize, Mtime: 11, ETag: "etag-after-move",
			},
			Baseline: baseline,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "doc.txt", "old")
	baseline := moveTestFileBaseline(t, e, syncRoot, "doc.txt")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		Path:    "moved-doc.txt",
		OldPath: "doc.txt",
		ItemID:  "item-moved",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "moved-doc.txt",
			Remote: &syncdomain.RemoteState{
				ItemID: "item-moved", ItemType: syncdomain.ItemTypeFile,
				Hash: "edited-remote-hash", Size: 99, Mtime: 12, ETag: "etag-edited",
			},
			Baseline: baseline,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	items := &syncfixture.ExecutorMockItemClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, _, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-swap", ETag: "etag-moved"}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "secret.txt",
		OldPath: "vim.swp",
		ItemID:  "item-swap",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path: "vim.swp",
			Baseline: &syncdomain.BaselineEntry{
				Path: "vim.swp", ItemID: "item-swap", ItemType: syncdomain.ItemTypeFile,
				LocalHash: "swap-hash", LocalSize: 5, LocalSizeKnown: true, LocalMtime: 100,
				LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
				RemoteHash: "swap-hash", RemoteSize: 5, RemoteSizeKnown: true,
			},
		},
		MovedLocal: &syncdomain.LocalState{
			ItemType: syncdomain.ItemTypeFile, Size: 5, Mtime: 100,
			LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "deep/er",
		CreateSide: syncdomain.CreateLocal,
		ItemID:     "item-er",
		DriveID:    driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path:   "deep/er",
			Remote: &syncdomain.RemoteState{ItemID: "item-er", ItemType: syncdomain.ItemTypeFolder},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)

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

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			return &graph.Item{ID: "item-" + name, Name: name, IsFolder: true}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "deep",
		CreateSide: syncdomain.CreateRemote,
		DriveID:    driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path:  "deep",
			Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder, LocalDevice: 3, LocalInode: 4, LocalHasIdentity: true},
		},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.True(t, o.LocalHasIdentity)
	assert.Equal(t, uint64(4), o.LocalInode)
}

// Validates: R-2.8.10, R-2.17.1
func TestExecutor_RemoteMove_FolderETagChurnFromChildrenDoesNotSupersede(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	var sentIfMatch string
	items := &syncfixture.ExecutorMockItemClient{
		// The folder's eTag moved on when children were uploaded into it
		// after its baseline/remote row was recorded.
		GetItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-dir", DriveID: driveID, IsFolder: true, ETag: `"etag-after-children"`}, nil
		},
		MoveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, ifMatch string) (*graph.Item, error) {
			sentIfMatch = ifMatch
			return &graph.Item{ID: "item-dir", IsFolder: true, ETag: `"etag-moved"`}, nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "movedst",
		OldPath: "movesrc",
		ItemID:  "item-dir",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "movesrc",
			Remote: &syncdomain.RemoteState{
				DriveID: driveID, ItemID: "item-dir", ItemType: syncdomain.ItemTypeFolder, ETag: `"etag-at-create"`,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Empty(t, sentIfMatch, "a folder move must not be conditional on a churning folder eTag")
}

// Validates: R-2.8.10
func TestExecutor_RemoteMove_FileETagMismatchStillSupersedes(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-file", DriveID: driveID, ETag: `"etag-changed"`}, nil
		},
		MoveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, _ string) (*graph.Item, error) {
			assert.Fail(t, "a changed file must not be moved")
			return nil, errors.New("unexpected move")
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "b.txt",
		OldPath: "a.txt",
		ItemID:  "item-file",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path:   "a.txt",
			Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item-file", ItemType: syncdomain.ItemTypeFile, ETag: `"etag-planned"`},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.10, R-2.17.2
func TestExecutor_RemoteMove_ItemAlreadyAtDestinationSucceedsWithoutMoving(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		// A parent folder move already carried the item to the destination.
		GetItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{
				ID: "item-inner", DriveID: driveID, IsFolder: true, Name: "inner",
				ParentPath: "movedst", ParentPathKnown: true, ETag: `"etag-after-parent-move"`,
			}, nil
		},
		MoveItemIfMatchFn: func(_ context.Context, _ driveid.ID, _, _, _, _ string) (*graph.Item, error) {
			assert.Fail(t, "an item already at its destination must not be moved again")
			return nil, errors.New("unexpected move")
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "movedst/inner",
		OldPath: "movesrc/inner",
		ItemID:  "item-inner",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path:   "movesrc/inner",
			Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item-inner", ItemType: syncdomain.ItemTypeFolder, ETag: `"etag-planned"`},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, "movedst/inner", o.Path)
	assert.Equal(t, `"etag-after-parent-move"`, o.ETag)
}

// Validates: R-2.17.9
//
// A local move that replaces its destination (OneDrive moved an item over a
// file it deleted) replaces only the version the plan saw; a destination that
// changed since planning is stale and stays untouched. The version replaced
// leaves the sync folder once, by the local delete rule: into the trash when
// the drive has one, where it is the destination's own file and is there
// exactly once, and removed otherwise. The rename then replaces nothing: the
// destination name holds the moved file itself.
func TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		destination string
		wantReplace bool
		withTrash   bool
	}{
		{name: "unchanged destination is replaced", destination: "bravo\n", wantReplace: true},
		{name: "changed destination is stale", destination: "edited after planning\n", wantReplace: false},
		{name: "unchanged destination goes to the trash", destination: "bravo\n", wantReplace: true, withTrash: true},
		{name: "changed destination is stale with a trash", destination: "edited after planning\n", withTrash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
			if tc.withTrash {
				cfg.trash = trash
			}
			e := NewExecution(cfg, syncfixture.EmptyBaseline())
			syncfixture.WriteExecTestFile(t, syncRoot, "a.txt", "alpha\n")
			syncfixture.WriteExecTestFile(t, syncRoot, "b.txt", "bravo\n")
			planned := plannedDestination(t, e, syncRoot, "b.txt")
			if tc.destination != "bravo\n" {
				syncfixture.WriteExecTestFile(t, syncRoot, "b.txt", tc.destination)
			}
			moved, displaced := syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "a.txt")), syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "b.txt"))

			action := &syncdomain.Action{
				Type:               syncdomain.ActionLocalMove,
				Path:               "b.txt",
				OldPath:            "a.txt",
				ReplaceLocalTarget: true,
				View: &syncdomain.PathView{
					Path:     "b.txt",
					Local:    planned,
					Baseline: &syncdomain.BaselineEntry{Path: "a.txt", ItemType: syncdomain.ItemTypeFile},
				},
			}

			o := e.ExecuteMove(t.Context(), action)
			data, readErr := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
			require.NoError(t, readErr)
			if !tc.wantReplace {
				syncfixture.RequireOutcomeFailure(t, &o)
				require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
				assert.Equal(t, tc.destination, string(data), "a changed destination is never replaced")
				assert.FileExists(t, filepath.Join(syncRoot, "a.txt"))
				entries, err := trash.List()
				require.NoError(t, err)
				assert.Empty(t, entries, "a destination that is not replaced does not go to the trash")
				return
			}
			syncfixture.RequireOutcomeSuccess(t, &o)
			assert.Equal(t, "alpha\n", string(data))
			assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
			assert.Equal(t, moved, syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "b.txt")), "b.txt is the moved file itself")
			assert.Equal(t, []string{"b.txt"}, syncfixture.LocalNames(t, syncRoot), "nothing is left beside it")
			if tc.withTrash {
				kept := syncfixture.RequireTrashHoldsOnce(t, trash, "b.txt", "bravo\n")
				assert.Equal(t, displaced, syncfixture.EntryIdentity(t, kept.ItemPath), "the trash holds the destination's own file, not a copy")
			}
		})
	}
}

// Validates: R-2.17.10
//
// A directory's conflict copy is a rename of the directory to a free
// conflict name (D-059a): the directory and what it holds move to the copy's
// name, and nothing is left at the old one.
func TestExecutor_ConflictCopy_DirectorySourceIsRenamed(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, filepath.Join("thing", "child.txt"), "child")

	o := e.ExecuteConflictCopy(t.Context(), &syncdomain.Action{Type: syncdomain.ActionConflictCopy, Path: "thing", View: &syncdomain.PathView{Path: "thing"}})
	syncfixture.RequireOutcomeSuccess(t, &o)

	_, err := os.Lstat(filepath.Join(syncRoot, "thing"))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing of any kind is left at the old name")
	assert.Contains(t, o.OldPath, "thing.conflict-")
	assert.FileExists(t, filepath.Join(syncRoot, o.OldPath, "child.txt"))
}

// Validates: R-2.17.10
//
// A directory's conflict copy never lands on something already at its name:
// an empty directory the user (or another copy) made at the first candidate
// survives, and the copy takes the next ordinal. A plain rename would have
// replaced the empty directory silently.
func TestExecutor_ConflictCopy_DirectoryNeverReplacesAnEmptyDirectoryAtItsName(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cfg.nowFunc = func() time.Time { return fixed }
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, filepath.Join("thing", "child.txt"), "child")
	firstCandidate := conflictCopyDirPathWithOrdinal(filepath.Join(syncRoot, "thing"), fixed, 1)
	require.NoError(t, os.Mkdir(firstCandidate, 0o700))
	marker := filepath.Join(firstCandidate, ".keep")

	o := e.ExecuteConflictCopy(t.Context(), &syncdomain.Action{Type: syncdomain.ActionConflictCopy, Path: "thing", View: &syncdomain.PathView{Path: "thing"}})
	syncfixture.RequireOutcomeSuccess(t, &o)

	assert.DirExists(t, firstCandidate, "the existing empty directory survives")
	assert.NoFileExists(t, marker)
	entries, err := os.ReadDir(firstCandidate)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was renamed onto it")
	assert.Equal(t, filepath.Base(conflictCopyDirPathWithOrdinal(filepath.Join(syncRoot, "thing"), fixed, 2)), filepath.Base(o.OldPath))
	assert.FileExists(t, filepath.Join(syncRoot, o.OldPath, "child.txt"))
}
