package syncexec

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

		// Invalid OneDrive names.
		{"desktop.ini", true},   // reserved name
		{"~$doc.docx", true},    // starts with ~$ (tilde + always excluded prefix)
		{"CON", true},           // reserved device name
		{"file.", true},         // trailing dot
		{" leadingspace", true}, // leading space

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

	items := &executorMockItemClient{}
	dl := &executorMockDownloader{}
	ul := &executorMockUploader{}

	cfg := NewExecutorConfig(items, dl, ul, syncRoot, driveID, logger)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })
	e := NewExecution(cfg, synctest.EmptyBaseline())

	return e, syncRoot
}

// Validates: R-6.2.4
func TestDeleteLocalFolder_DSStoreOnly_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	// Create directory with .DS_Store.
	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, outcome)

	// Directory should be gone.
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestDeleteLocalFolder_TmpFilesOnly_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp.tmp"), []byte("tmp"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "edit.swp"), []byte("swp"), 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

// Validates: R-6.2.4
func TestDeleteLocalFolder_UnknownFile_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "important.txt"), []byte("data"), 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, outcome)
	assert.Contains(t, outcome.Error.Error(), "important.txt")
}

func TestDeleteLocalFolder_MixedDisposableAndUnknown_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "realfile.doc"), []byte("content"), 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, outcome)
	assert.Contains(t, outcome.Error.Error(), "realfile.doc")

	// Directory still exists.
	_, err := os.Stat(dir)
	assert.NoError(t, err)
}

func TestDeleteLocalFolder_AppleDoubleFiles_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "._photo.jpg"), []byte{0}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestDeleteLocalFolder_DisposableDirWithNonDisposableChild_Fails(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "important.txt"), []byte("data"), 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeFailure(t, outcome)
	assert.Contains(t, outcome.Error.Error(), "__MACOSX/important.txt")
}

func TestDeleteLocalFolder_DisposableDirAllDisposableChildren_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "folder")
	macDir := filepath.Join(dir, "__MACOSX")
	require.NoError(t, os.MkdirAll(macDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, "._photo.jpg"), []byte{0}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(macDir, ".DS_Store"), []byte{0}, 0o644))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "folder", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestFindNonDisposable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// All disposable.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{0}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "._foo"), []byte{0}, 0o644))
	assert.Empty(t, FindNonDisposable(dir))

	// Add a non-disposable file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("data"), 0o644))
	assert.Equal(t, "real.txt", FindNonDisposable(dir))
}

func TestDeleteLocalFolder_EmptyDir_Succeeds(t *testing.T) {
	t.Parallel()

	e, syncRoot := newDeleteTestExecutor(t)

	dir := filepath.Join(syncRoot, "empty")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	action := &synctypes.Action{Type: synctypes.ActionLocalDelete, Path: "empty", ItemID: "id1"}
	outcome := e.DeleteLocalFolder(action, dir)

	requireOutcomeSuccess(t, outcome)

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}
