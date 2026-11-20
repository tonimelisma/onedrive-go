package syncexec

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.4.8, R-2.4.6
//
// The directory conflict copy renames through descriptors opened inside the
// sync folder. Its caller refuses a symlinked ancestor, but a folder replaced
// by a symlink after that check is the state this test builds: "alias" leads
// out of the sync folder, and the copy is asked to rename a directory beneath
// it.
func TestExecutor_DirectoryConflictCopy_ParentSwappedForSymlinkStaysInside(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	outsideDir := filepath.Join(f.outside, "target", "photos")
	require.NoError(t, os.MkdirAll(outsideDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "child.txt"), []byte(outsideContent), 0o600))

	action := &syncdomain.Action{Type: syncdomain.ActionConflictCopy, Path: "alias/photos", View: &syncdomain.PathView{Path: "alias/photos"}}
	o := f.exec.renameToConflictCopy(action, action.Path, action.Path, conflictCopyDirPathWithOrdinal)
	syncfixture.RequireOutcomeFailure(t, &o)

	f.requireTargetIntact(t, filepath.Join("photos", "child.txt"))
	assert.Equal(t, []string{"photos"}, syncfixture.LocalNames(t, filepath.Join(f.outside, "target")),
		"no conflict copy lands outside the sync folder")
}

// Validates: R-2.17.10, R-2.3.5
//
// A folder that vanishes before its conflict copy's rename gets the outcome a
// vanished file gets: nothing is left to preserve, so the copy succeeds with
// the path it was asked about and its parent, and nothing is created.
func TestExecutor_DirectoryConflictCopy_VanishedSourceIsTheVanishedFileOutcome(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	action := &syncdomain.Action{Type: syncdomain.ActionConflictCopy, Path: "gone", ItemID: "item-gone", View: &syncdomain.PathView{Path: "gone"}}

	vanishedFile := e.ExecuteConflictCopy(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &vanishedFile)

	vanishedFolder := e.renameToConflictCopy(action, action.Path, action.Path, conflictCopyDirPathWithOrdinal)
	syncfixture.RequireOutcomeSuccess(t, &vanishedFolder)
	assert.Equal(t, vanishedFile, vanishedFolder)
	assert.Equal(t, action.Path, vanishedFolder.OldPath)

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created for a vanished source")
}

// Validates: R-2.17.10, R-2.3.5
//
// A rename that fails as missing because the copy's folder is missing, not
// the source, is a failure with the rename's own error: only a source a lstat
// through the sync tree finds absent is the vanished outcome. It holds for a
// file and a folder alike, and the source stays where it was.
func TestExecutor_ConflictCopy_MissingDestinationFolderIsNotAVanishedSource(t *testing.T) {
	t.Parallel()

	sources := []struct {
		name          string
		create        func(t *testing.T, syncRoot string)
		candidateName func(string, time.Time, int) string
	}{
		{name: "a file", candidateName: conflictCopyPathWithOrdinal, create: func(t *testing.T, syncRoot string) {
			t.Helper()
			syncfixture.WriteExecTestFile(t, syncRoot, "present", "local edit")
		}},
		{name: "a folder", candidateName: conflictCopyDirPathWithOrdinal, create: func(t *testing.T, syncRoot string) {
			t.Helper()
			syncfixture.WriteExecTestFile(t, syncRoot, filepath.Join("present", "child.txt"), "child")
		}},
	}
	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			t.Parallel()

			e, syncRoot := newConflictCopyExecutor(t)
			src.create(t, syncRoot)
			action := conflictCopyAction("present")

			o := e.renameToConflictCopy(action, "present", filepath.Join("missing", "present"), src.candidateName)
			syncfixture.RequireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, os.ErrNotExist, "the rename's own error is reported")
			assert.Equal(t, []string{"present"}, syncfixture.LocalNames(t, syncRoot))
		})
	}
}

// Validates: R-2.17.10
//
// When the rename fails as missing and the lstat that would tell a vanished
// source fails another way, the failure carries both reasons, on one line, as
// the console prints a failure reason.
func TestExecutor_ConflictCopy_UncheckableSourceAfterAMissingRenameKeepsBothReasons(t *testing.T) {
	t.Parallel()

	e, syncRoot := newConflictCopyExecutor(t)
	syncfixture.WriteExecTestFile(t, syncRoot, filepath.Join("present", "child.txt"), "child")
	shim := synctest.NewErrnoShim(synctest.ErrnoRule{Op: synctest.FSOpStat, Path: "present", Errno: syscall.EACCES})
	e.syncTree = e.syncTree.Interpose(shim.Wrap)

	o := e.renameToConflictCopy(conflictCopyAction("present"), "present", filepath.Join("missing", "present"),
		conflictCopyDirPathWithOrdinal)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, os.ErrNotExist, "the rename's own error is reported")
	require.ErrorIs(t, o.Error, syscall.EACCES, "the check's reason is reported too")
	assert.NotContains(t, o.Error.Error(), "\n", "both reasons are on one line")
	assert.DirExists(t, filepath.Join(syncRoot, "present"))
}

// Validates: R-2.17.10
//
// A conflict copy whose source cannot be checked fails with the reason and
// creates nothing beside the source.
func TestExecutor_ConflictCopy_UncheckableSourceFailsWithItsReason(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, filepath.Join("thing", "child.txt"), "child")
	shim := synctest.NewErrnoShim(synctest.ErrnoRule{Op: synctest.FSOpStat, Path: "thing", Errno: syscall.EACCES})
	e.syncTree = e.syncTree.Interpose(shim.Wrap)

	o := e.ExecuteConflictCopy(t.Context(), &syncdomain.Action{Type: syncdomain.ActionConflictCopy, Path: "thing", View: &syncdomain.PathView{Path: "thing"}})
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syscall.EACCES)

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing is created beside the source")
	assert.Equal(t, "thing", entries[0].Name())
	assert.FileExists(t, filepath.Join(syncRoot, "thing", "child.txt"))
}
