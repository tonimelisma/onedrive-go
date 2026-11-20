package syncexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ---------------------------------------------------------------------------
// IsDisposable tests
// ---------------------------------------------------------------------------

// Validates: R-2.4.14
func TestIsDisposable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		// OS junk.
		{".DS_Store", true},
		{".ds_store", true},
		{"Thumbs.db", true},
		{"thumbs.db", true},
		{"__MACOSX", true},
		{"__macosx", true},

		// Apple resource forks.
		{"._photo.jpg", true},
		{"._document.pdf", true},

		// Editor temps / partial downloads.
		{"file.tmp", true},
		{"file.swp", true},
		{"file.partial", true},
		{"file.crdownload", true},
		{"~backup", true},
		{".~lock.file", true},
		// Office owner files are junk under P-2 (they are lock files that
		// churn and confuse other devices).
		{"~$doc.docx", true},
		{"app.db-wal", true},

		// Invalid OneDrive names are user-actionable issues, not disposable.
		{"desktop.ini", false},
		{"CON", false},
		{"file.", false},
		{" leadingspace", false},

		// Normal files — NOT disposable.
		{"important.txt", false},
		{"photo.jpg", false},
		{"document.pdf", false},
		{"README.md", false},
		{".gitignore", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isDisposable(tt.name, false), "IsDisposable(%q)", tt.name)
		})
	}

	// The file-only rules never make a user's folder disposable.
	for _, folder := range []string{"~Projects", "notes~", "4913"} {
		assert.False(t, isDisposable(folder, true), "folder %q is the user's", folder)
	}
	assert.True(t, isDisposable("$RECYCLE.BIN", true), "trash folders stay junk")
	assert.True(t, isDisposable(".Trash-1000", true))
}

// ---------------------------------------------------------------------------
// DeleteLocalFolder with disposable files tests
// ---------------------------------------------------------------------------

func newDeleteTestExecutor(t *testing.T) (*Executor, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	items := &syncfixture.ExecutorMockItemClient{}
	dl := &syncfixture.ExecutorMockDownloader{}
	ul := &syncfixture.ExecutorMockUploader{}

	cfg := NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, nil)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetContentFilter(syncdomain.ContentFilterConfig{IgnoreJunkFiles: true})
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	return e, syncRoot
}

func TestDeleteLocalFolder_JunkFileWhenIgnoreJunkDisabled_Fails(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := NewExecutorConfig(
		&syncfixture.ExecutorMockItemClient{},
		&syncfixture.ExecutorMockDownloader{},
		&syncfixture.ExecutorMockUploader{},
		syncTree,
		driveID,
		logger,
		nil,
	)
	cfg.SetTransferMgr(driveops.NewTransferManager(&syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, nil, logger))
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), ".DS_Store")
}

// Validates: R-6.2.4
func TestDeleteLocalFolder_DSStoreOnly_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	// Create directory with .DS_Store.
	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)

	// Directory should be gone.
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestExecuteRemoteDelete_DoesNotUsePathConvergenceDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "remote-item-id", itemID)
			return &graph.Item{
				ID:       "remote-item-id",
				ETag:     "etag-1",
				DriveID:  driveID,
				ParentID: "parent-id",
				Name:     "report.txt",
				Size:     1,
			}, nil
		},
		DeleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			assert.Equal(t, "remote-item-id", itemID)
			return nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg := NewExecutorConfig(items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, syncTree, driveID, logger, pathConvergence)
	cfg.SetTransferMgr(driveops.NewTransferManager(&syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{}, nil, logger))
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:   syncdomain.ActionRemoteDelete,
		Path:   "docs/report.txt",
		ItemID: "remote-item-id",
		View: &syncdomain.PathView{
			Path: "docs/report.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "remote-item-id",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     "etag-1",
				Size:     1,
			},
		},
	}

	outcome := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &outcome)
	assert.Empty(t, pathConvergence.deleteResolvedCalls)
	assert.Empty(t, pathConvergence.permanentDeletePathCalls)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_NotFoundPreflightReturnsStalePreconditionAndDoesNotDelete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "remote-item-id", itemID)
			return nil, graph.ErrNotFound
		},
		DeleteItemFn: func(context.Context, driveid.ID, string) error {
			require.FailNow(t, "remote delete should not run after stale preflight")
			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "docs/report.txt",
		ItemID:  "remote-item-id",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "docs/report.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "remote-item-id",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     "etag-1",
			},
		},
	}

	outcome := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_ETagMismatchPreflightReturnsStalePreconditionAndDoesNotDelete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "remote-item-id", itemID)
			return &graph.Item{
				ID:      "remote-item-id",
				DriveID: driveID,
				ETag:    "etag-current",
			}, nil
		},
		DeleteItemFn: func(context.Context, driveid.ID, string) error {
			require.FailNow(t, "remote delete should not run after stale preflight")
			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "docs/report.txt",
		ItemID:  "remote-item-id",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "docs/report.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "remote-item-id",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     "etag-planned",
			},
		},
	}

	outcome := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_TransientPreflightFailureIsOrdinaryFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			assert.Equal(t, "remote-item-id", itemID)
			return nil, assert.AnError
		},
		DeleteItemFn: func(context.Context, driveid.ID, string) error {
			require.FailNow(t, "remote delete should not run after failed preflight")
			return nil
		},
	}

	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteDelete,
		Path:    "docs/report.txt",
		ItemID:  "remote-item-id",
		DriveID: driveID,
		View: &syncdomain.PathView{
			Path: "docs/report.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "remote-item-id",
				ItemType: syncdomain.ItemTypeFile,
				ETag:     "etag-planned",
			},
		},
	}

	outcome := e.ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, assert.AnError)
	assert.NotErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
}

func TestExecuteLocalDelete_SymlinkAliasDeletesOnlyLink(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "target.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("target"), 0o600))
	require.NoError(t, os.Symlink(targetPath, filepath.Join(syncRoot, "link.txt")))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "link.txt", ItemID: "id-link"}
	outcome := e.ExecuteLocalDelete(t.Context(), action)

	syncfixture.RequireOutcomeSuccess(t, &outcome)
	_, linkErr := os.Lstat(filepath.Join(syncRoot, "link.txt"))
	assert.True(t, os.IsNotExist(linkErr))
	assert.FileExists(t, targetPath)
}

func TestExecuteLocalDelete_BlocksDescendantUnderSymlinkedDirectory(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "child.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("target"), 0o600))
	require.NoError(t, os.Symlink(targetDir, filepath.Join(syncRoot, "linkdir")))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "linkdir/child.txt", ItemID: "id-child"}
	outcome := e.ExecuteLocalDelete(t.Context(), action)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "linkdir is a symbolic link")
	assert.FileExists(t, targetPath)
	_, linkErr := os.Lstat(filepath.Join(syncRoot, "linkdir"))
	require.NoError(t, linkErr)
}

func TestDeleteLocalFolder_TmpFilesOnly_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp.tmp"), []byte("tmp"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "edit.swp"), []byte("swp"), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

// Validates: R-6.2.4
func TestDeleteLocalFolder_UnknownFile_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "important.txt"), []byte("data"), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "important.txt")
}

func TestDeleteLocalFolder_MixedDisposableAndUnknown_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "realfile.doc"), []byte("content"), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "realfile.doc")

	// Directory still exists.
	_, err := os.Stat(dir)
	assert.NoError(t, err)
}

func TestDeleteLocalFolder_AppleDoubleFiles_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "._photo.jpg"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

// A junk-named folder never syncs, so a non-junk file inside it is content
// sync does not own: the folder delete keeps it (D-012) instead of either
// deleting it or failing forever.
func TestDeleteLocalFolder_DisposableDirWithNonDisposableChildIsKept(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "important.txt"), []byte("data"), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)
	assert.FileExists(t, filepath.Join(macDir, "important.txt"), "content inside an ignored folder is never deleted")
}

// A visible file still blocks the folder delete with a reason.
func TestDeleteLocalFolder_VisibleChildBlocksDelete(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "important.txt"), []byte("data"), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "important.txt")
	assert.FileExists(t, filepath.Join(dir, "important.txt"))
}

func TestDeleteLocalFolder_DisposableDirAllDisposableChildren_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "._photo.jpg"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, ".DS_Store"), []byte{0}, 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestFindNonDisposable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	// All disposable.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "._foo"), []byte{0}, 0o600))
	assert.Empty(t, findNonDisposable(tree, "", isDisposable))

	// Add a non-disposable file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("data"), 0o600))
	assert.Equal(t, "real.txt", findNonDisposable(tree, "", isDisposable))
}

func TestDeleteLocalFolder_EmptyDir_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "empty")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	action := &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "empty", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(t.Context(), action, dir)

	syncfixture.RequireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

// Validates: R-2.17.2
func TestDeleteLocalFolder_JunkRemovalFailureIsNamedInTheError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)
	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "._photo.jpg"), []byte{0}, 0o600))
	require.NoError(t, os.Chmod(macDir, 0o500))                      //nolint:gosec // a read-only folder makes junk removal fail
	t.Cleanup(func() { assert.NoError(t, os.Chmod(macDir, 0o700)) }) //nolint:gosec // restores the folder's permissions

	outcome := e.DeleteLocalFolder(t.Context(), &syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "folder", ItemID: "id1"}, dir)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "after failing to remove junk inside it")
}
