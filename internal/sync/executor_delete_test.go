package sync

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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ---------------------------------------------------------------------------
// isDisposable tests
// ---------------------------------------------------------------------------

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

		// Invalid OneDrive names are user-actionable issues, not disposable.
		{"desktop.ini", false},
		{"~$doc.docx", false},
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
			assert.Equal(t, tt.want, IsDisposable(tt.name), "IsDisposable(%q)", tt.name)
		})
	}
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

	items := &executorMockItemClient{}
	dl := &executorMockDownloader{}
	ul := &executorMockUploader{}

	cfg := NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, nil)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })
	e := NewExecution(cfg, emptyBaseline())

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
		&executorMockItemClient{},
		&executorMockDownloader{},
		&executorMockUploader{},
		syncTree,
		driveID,
		logger,
		nil,
	)
	cfg.SetTransferMgr(driveops.NewTransferManager(&executorMockDownloader{}, &executorMockUploader{}, nil, logger))
	e := NewExecution(cfg, emptyBaseline())

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, &outcome)
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

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, &outcome)

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

	items := &executorMockItemClient{
		deleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			assert.Equal(t, "remote-item-id", itemID)
			return nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}

	cfg := NewExecutorConfig(items, &executorMockDownloader{}, &executorMockUploader{}, syncTree, driveID, logger, pathConvergence)
	cfg.SetTransferMgr(driveops.NewTransferManager(&executorMockDownloader{}, &executorMockUploader{}, nil, logger))
	e := NewExecution(cfg, emptyBaseline())

	action := &Action{
		Type:   ActionRemoteDelete,
		Path:   "docs/report.txt",
		ItemID: "remote-item-id",
		View: &PathView{
			Remote: &RemoteState{ETag: "etag-1"},
		},
	}

	outcome := e.ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeSuccess(t, &outcome)
	assert.Empty(t, pathConvergence.deleteResolvedCalls)
	assert.Empty(t, pathConvergence.permanentDeletePathCalls)
}

func TestExecuteLocalDelete_SymlinkAliasDeletesOnlyLink(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "target.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("target"), 0o600))
	require.NoError(t, os.Symlink(targetPath, filepath.Join(syncRoot, "link.txt")))

	action := &Action{Type: ActionLocalDelete, Path: "link.txt", ItemID: "id-link"}
	outcome := e.ExecuteLocalDelete(t.Context(), action)

	requireOutcomeSuccess(t, &outcome)
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

	action := &Action{Type: ActionLocalDelete, Path: "linkdir/child.txt", ItemID: "id-child"}
	outcome := e.ExecuteLocalDelete(t.Context(), action)

	requireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "symlink boundary linkdir")
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

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, &outcome)

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

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "important.txt")
}

func TestDeleteLocalFolder_MixedDisposableAndUnknown_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "realfile.doc"), []byte("content"), 0o600))

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, &outcome)
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

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestDeleteLocalFolder_DisposableDirWithNonDisposableChild_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "important.txt"), []byte("data"), 0o600))

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, &outcome)
	assert.Contains(t, outcome.Error.Error(), "__MACOSX/important.txt")
}

func TestDeleteLocalFolder_DisposableDirAllDisposableChildren_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "._photo.jpg"), []byte{0}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, ".DS_Store"), []byte{0}, 0o600))

	action := &Action{Type: ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, &outcome)

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
	assert.Empty(t, FindNonDisposable(tree, ""))

	// Add a non-disposable file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("data"), 0o600))
	assert.Equal(t, "real.txt", FindNonDisposable(tree, ""))
}

func TestDeleteLocalFolder_EmptyDir_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "empty")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	action := &Action{Type: ActionLocalDelete, Path: "empty", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, &outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}
