package sync

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func conflictCopyNames(t *testing.T, syncRoot string) []string {
	t.Helper()

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)

	var names []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			names = append(names, entry.Name())
		}
	}

	return names
}

// conflictCopyClock is the fixed second every conflict copy of these tests is
// made in, so two copies of one name compete for the same candidates.
func conflictCopyClock() time.Time {
	return time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
}

func newConflictCopyExecutor(t *testing.T) (*executor, string) {
	t.Helper()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetNowFunc(conflictCopyClock)

	return newExecution(cfg, emptyBaseline()), syncRoot
}

func conflictCopyAction(path string) *Action {
	return &Action{
		Type:    ActionConflictCopy,
		Path:    path,
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path:   path,
			Local:  &localState{ItemType: ItemTypeFile},
			Remote: &remoteState{ItemID: "item1", ETag: "etag1"},
		},
	}
}

func requireExecFileContent(t *testing.T, syncRoot, rel, want string) {
	t.Helper()

	data, err := localpath.ReadFile(filepath.Join(syncRoot, rel))
	require.NoError(t, err)
	assert.Equal(t, want, string(data))
}

// Validates: R-2.3.5
//
// A conflict copy never takes a name by finding it free and then renaming
// onto it: rename silently replaces whatever sits at the destination. Two
// conflict copies of one name resolved in the same second are the reachable
// case, and the fixed test clock reproduces it exactly: each copy takes its
// own name, and both keep their content.
func TestExecutor_ConflictCopy_SameSecondCopiesTakeTheirOwnNames(t *testing.T) {
	t.Parallel()

	e, syncRoot := newConflictCopyExecutor(t)

	writeExecTestFile(t, syncRoot, "clash.txt", "first")
	first := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("clash.txt"))
	requireOutcomeSuccess(t, &first)
	writeExecTestFile(t, syncRoot, "clash.txt", "second")
	second := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("clash.txt"))
	requireOutcomeSuccess(t, &second)

	assert.NotEqual(t, first.OldPath, second.OldPath,
		"two conflict copies resolved in the same second must not share a destination")
	assert.Equal(t, conflictCopyPathWithOrdinal("clash.txt", conflictCopyClock(), 2), second.OldPath)
	requireExecFileContent(t, syncRoot, first.OldPath, "first")
	requireExecFileContent(t, syncRoot, second.OldPath, "second")
}

// Validates: R-2.3.5
//
// A conflict copy whose rename fails leaves no name behind: nothing is
// created before the rename, so no empty file can read as a truncated
// conflict copy.
func TestExecutor_ConflictCopy_FailedRenameLeavesNoConflictCopyName(t *testing.T) {
	t.Parallel()

	e, syncRoot := newConflictCopyExecutor(t)
	writeExecTestFile(t, syncRoot, filepath.Join("docs", "a.txt"), "original")
	shim := synctest.NewErrnoShim(
		synctest.ErrnoRule{Op: synctest.FSOpOpen, Path: "docs", Errno: syscall.EACCES},
		synctest.ErrnoRule{Op: synctest.FSOpRename, Path: "docs", Errno: syscall.EACCES},
	)
	e.syncTree = e.syncTree.Interpose(shim.Wrap)

	o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("docs/a.txt"))
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syscall.EACCES)
	assert.Empty(t, conflictCopyNames(t, filepath.Join(syncRoot, "docs")))
	requireExecFileContent(t, syncRoot, filepath.Join("docs", "a.txt"), "original")
}

// Validates: R-2.3.5
//
// A completed conflict copy leaves exactly one file, holding the original
// content.
func TestExecutor_ConflictCopy_LeavesExactlyOneCopyHoldingTheOriginal(t *testing.T) {
	t.Parallel()

	e, syncRoot := newConflictCopyExecutor(t)
	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local edit worth keeping")

	o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("exec-conflict.txt"))
	requireOutcomeSuccess(t, &o)

	names := conflictCopyNames(t, syncRoot)
	require.Len(t, names, 1, "a conflict copy must leave exactly one file")
	requireExecFileContent(t, syncRoot, names[0], "local edit worth keeping")
}

// Validates: R-2.3.5, R-6.4.8
//
// A file conflict copy whose first name is taken, by a file or by an empty
// folder, takes the next ordinal and leaves what took the name as it was.
func TestExecutor_ConflictCopy_TakenNameMovesOnToTheNextOrdinal(t *testing.T) {
	t.Parallel()

	takers := []struct {
		name  string
		take  func(t *testing.T, path string)
		check func(t *testing.T, path string)
	}{
		{
			name: "a file",
			take: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("taker"), 0o600))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				data, err := localpath.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, "taker", string(data))
			},
		},
		{
			name: "an empty folder",
			take: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Mkdir(path, 0o700))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				entries, err := os.ReadDir(path)
				require.NoError(t, err)
				assert.Empty(t, entries, "nothing was moved into or onto the folder")
			},
		},
	}
	for _, taker := range takers {
		t.Run(taker.name, func(t *testing.T) {
			t.Parallel()

			e, syncRoot := newConflictCopyExecutor(t)
			writeExecTestFile(t, syncRoot, "a.txt", "local edit")
			firstName := conflictCopyPathWithOrdinal("a.txt", conflictCopyClock(), 1)
			taker.take(t, filepath.Join(syncRoot, firstName))

			o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("a.txt"))
			requireOutcomeSuccess(t, &o)
			assert.Equal(t, conflictCopyPathWithOrdinal("a.txt", conflictCopyClock(), 2), o.OldPath)
			requireExecFileContent(t, syncRoot, o.OldPath, "local edit")
			taker.check(t, filepath.Join(syncRoot, firstName))
		})
	}
}

// Validates: R-2.3.5, R-2.17.10, R-6.4.8
//
// On a system with neither the no-replace flag nor hard links the conflict
// copy still renames the entry itself: a folder moves, and a file keeps its
// inode, so a program writing to it keeps writing to the copy.
func TestExecutor_ConflictCopy_WithoutTheFlagOrLinksRenamesTheEntryItself(t *testing.T) {
	t.Parallel()

	t.Run("a folder", func(t *testing.T) {
		t.Parallel()

		e, syncRoot := newConflictCopyExecutor(t)
		e.syncTree = e.syncTree.WithoutNoReplaceFlagOrHardLinks()
		writeExecTestFile(t, syncRoot, filepath.Join("photos", "a.jpg"), "a")

		o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("photos"))
		requireOutcomeSuccess(t, &o)
		assert.Equal(t, conflictCopyDirPathWithOrdinal("photos", conflictCopyClock(), 1), o.OldPath)
		requireExecFileContent(t, syncRoot, filepath.Join(o.OldPath, "a.jpg"), "a")
		assert.NoDirExists(t, filepath.Join(syncRoot, "photos"))
	})

	t.Run("a file", func(t *testing.T) {
		t.Parallel()

		e, syncRoot := newConflictCopyExecutor(t)
		e.syncTree = e.syncTree.WithoutNoReplaceFlagOrHardLinks()
		writeExecTestFile(t, syncRoot, "a.txt", "local edit")
		before, err := os.Lstat(filepath.Join(syncRoot, "a.txt"))
		require.NoError(t, err)

		o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("a.txt"))
		requireOutcomeSuccess(t, &o)
		after, err := os.Lstat(filepath.Join(syncRoot, o.OldPath))
		require.NoError(t, err)
		assert.True(t, os.SameFile(before, after), "the file is renamed, not copied")
		assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	})
}

// Validates: R-2.3.5, R-6.4.8
//
// A conflict copy whose rename placed the copy but could not remove the
// original name succeeds: the copy is at its name, which is what the conflict
// copy is for, the original name still names the same file, and a warning
// names the name left behind.
func TestExecutor_ConflictCopy_OriginalNameThatCannotBeRemovedIsASavedCopy(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	e, syncRoot := newConflictCopyExecutor(t)
	capture, logger := synctest.NewLogCapture()
	e.logger = logger
	e.syncTree = e.syncTree.WithoutNoReplaceFlagAndUnremovableName("a.txt")
	writeExecTestFile(t, syncRoot, "a.txt", "local edit")

	o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("a.txt"))
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, conflictCopyPathWithOrdinal("a.txt", conflictCopyClock(), 1), o.OldPath)
	requireExecFileContent(t, syncRoot, o.OldPath, "local edit")
	requireExecFileContent(t, syncRoot, "a.txt", "local edit")
	warnings := capture.AtLeast(slog.LevelWarn)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "could not remove the original name a.txt")
}

// Validates: R-2.3.5
//
// A conflict copy that finds every same-second name taken fails, leaves the
// source and each name's holder as they were, and says how many names it
// tried and the second they share.
func TestExecutor_ConflictCopy_EveryNameTakenSaysHowManyAndWhichSecond(t *testing.T) {
	t.Parallel()

	e, syncRoot := newConflictCopyExecutor(t)
	writeExecTestFile(t, syncRoot, "a.txt", "local edit")
	for ordinal := 1; ordinal <= conflictCopyAttempts; ordinal++ {
		writeExecTestFile(t, syncRoot, conflictCopyPathWithOrdinal("a.txt", conflictCopyClock(), ordinal), "taken")
	}

	o := e.ExecuteConflictCopy(t.Context(), conflictCopyAction("a.txt"))
	requireOutcomeFailure(t, &o)
	require.ErrorContains(t, o.Error, fmt.Sprintf("all %d names", conflictCopyAttempts))
	require.ErrorContains(t, o.Error, conflictCopyClock().Format(conflictCopyStampLayout))
	requireExecFileContent(t, syncRoot, "a.txt", "local edit")
	assert.Len(t, conflictCopyNames(t, syncRoot), conflictCopyAttempts)
}
