package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// syncedContent is what the file a local delete test removes holds, as its
// baseline records it.
const syncedContent = "synced\n"

// localDeleteActionFor is the local delete of path, a file whose baseline
// records syncedContent.
func localDeleteActionFor(t *testing.T, path string) *Action {
	t.Helper()

	hash := hashContentQuickXor(t, syncedContent)
	return &Action{
		Type:    ActionLocalDelete,
		Path:    path,
		ItemID:  "item-" + path,
		DriveID: driveid.New(synctest.TestDriveID),
		View: &PathView{
			Path: path,
			Baseline: &BaselineEntry{
				Path: path, ItemID: "item-" + path, ItemType: ItemTypeFile,
				LocalHash: hash, RemoteHash: hash,
				LocalSize: int64(len(syncedContent)), LocalSizeKnown: true,
			},
		},
	}
}

// Validates: R-6.4.9
func TestDeleteLocalFile_MovesTheFileToTheTrash(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
	cfg.SetTrash(trash)
	writeExecTestFile(t, syncRoot, "docs/a.txt", syncedContent)

	outcome := NewExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "docs/a.txt"))

	requireOutcomeSuccess(t, &outcome)
	assert.NoFileExists(t, filepath.Join(syncRoot, "docs", "a.txt"))
	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "docs/a.txt", entries[0].Path)
	assert.Equal(t, localtrash.ReasonDeleted, entries[0].Reason)
	assert.Equal(t, int64(len(syncedContent)), entries[0].Size)
}

// The user saves a new version (an editor's write-then-rename) while sync is
// hashing the old one for a delete. The old shape hashed by path and then
// removed by path, unlinking the new content (D-010).
//
// Validates: R-6.2.4
func TestDeleteLocalFile_FileReplacedMidHashSurvives(t *testing.T) {
	t.Parallel()

	for _, withTrash := range []bool{true, false} {
		t.Run(map[bool]string{true: "trash", false: "no trash"}[withTrash], func(t *testing.T) {
			t.Parallel()

			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
			var trash *localtrash.Trash
			if withTrash {
				trash = localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
				cfg.SetTrash(trash)
			}
			target := writeExecTestFile(t, syncRoot, "a.txt", syncedContent)
			cfg.hashReader = func(ctx context.Context, r io.Reader) (string, error) {
				hash, err := driveops.ComputeQuickXorHashFromReaderContext(ctx, r)
				replacement := writeExecTestFile(t, syncRoot, "a.txt.save", "new edit\n")
				require.NoError(t, os.Rename(replacement, target))
				if err != nil {
					return "", fmt.Errorf("hashing in test: %w", err)
				}
				return hash, nil
			}

			outcome := NewExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "a.txt"))

			requireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
			data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
			require.NoError(t, err)
			assert.Equal(t, "new edit\n", string(data), "the new content must survive")
			if trash != nil {
				entries, listErr := trash.List()
				require.NoError(t, listErr)
				assert.Empty(t, entries)
			}
		})
	}
}

// hashRemovingTrash hashes as the executor does after removing the trash at
// trashDir: a trash whose entry folder is gone between its reservation and
// the move, as when the trash is emptied meanwhile.
func hashRemovingTrash(t *testing.T, trashDir string) func(context.Context, io.Reader) (string, error) {
	t.Helper()

	return func(ctx context.Context, r io.Reader) (string, error) {
		require.NoError(t, os.RemoveAll(trashDir))
		hash, err := driveops.ComputeQuickXorHashFromReaderContext(ctx, r)
		if err != nil {
			return "", fmt.Errorf("hashing in test: %w", err)
		}
		return hash, nil
	}
}

// A local delete whose trash fails is a failed delete, not a file already
// gone: when the trash entry's folder disappears between its reservation and
// the move, the delete fails instead of being superseded as a file that is
// already absent, and the file stays in the sync folder.
//
// Validates: R-6.4.9
func TestDeleteLocalFile_ATrashThatFailsIsAFailureNotAFileAlreadyGone(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trashDir := filepath.Join(t.TempDir(), "trash")
	cfg.SetTrash(localtrash.Open(trashDir, nil))
	cfg.hashReader = hashRemovingTrash(t, trashDir)
	writeExecTestFile(t, syncRoot, "docs/a.txt", syncedContent)

	outcome := NewExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "docs/a.txt"))

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, synctree.ErrDestinationFailed)
	require.NotErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
	requireExecFileContent(t, syncRoot, "docs/a.txt", syncedContent)
}

// A replacing local move whose trash fails fails: when the trash entry's
// folder disappears between its reservation and the move, the move is not
// read as one whose destination left on its own, to be superseded when its
// rename finds the name taken. It fails, the file at the destination stays,
// and the moved file stays at its source.
//
// Validates: R-6.4.9, R-2.17.9
func TestExecutor_LocalMove_ATrashThatFailsFailsTheReplacingMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trashDir := filepath.Join(t.TempDir(), "trash")
	cfg.SetTrash(localtrash.Open(trashDir, nil))
	cfg.hashReader = hashRemovingTrash(t, trashDir)
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "a.txt", "moving\n")
	writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
	planned := plannedDestination(t, e, syncRoot, "b.txt")

	o := e.ExecuteMove(t.Context(), &Action{
		Type: ActionLocalMove, Path: "b.txt", OldPath: "a.txt", ReplaceLocalTarget: true,
		View: &PathView{
			Path: "b.txt", Local: planned,
			Baseline: &BaselineEntry{Path: "a.txt", ItemType: ItemTypeFile},
		},
	})

	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, synctree.ErrDestinationFailed)
	require.NotErrorIs(t, o.Error, ErrActionPreconditionChanged)
	requireExecFileContent(t, syncRoot, "b.txt", "bravo\n")
	requireExecFileContent(t, syncRoot, "a.txt", "moving\n")
}

// reservationLosingItsFolder is a trash whose reservations fail as one does
// whose entry folder is removed while its record is written: with an error
// that matches os.ErrNotExist.
func reservationLosingItsFolder(t *testing.T) *localtrash.Trash {
	t.Helper()

	lost := &os.PathError{Op: "rename", Path: filepath.Join(t.TempDir(), "entry", "entry.json"), Err: syscall.ENOENT}
	return localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil).WithRecordWriteFailing(lost)
}

// A local delete whose trash entry cannot be reserved is a failed delete,
// not a file already gone: the reservation's not-exist is the trash's, so
// the delete fails instead of being superseded as one whose file is already
// absent, and the file stays in the sync folder.
//
// Validates: R-6.4.9
func TestDeleteLocalFile_AReservationThatFailsIsAFailureNotAFileAlreadyGone(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetTrash(reservationLosingItsFolder(t))
	writeExecTestFile(t, syncRoot, "docs/a.txt", syncedContent)

	outcome := NewExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "docs/a.txt"))

	requireOutcomeFailure(t, &outcome)
	require.NotErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
	require.NotErrorIs(t, outcome.Error, os.ErrNotExist)
	require.ErrorContains(t, outcome.Error, "reserving a trash entry")
	requireExecFileContent(t, syncRoot, "docs/a.txt", syncedContent)
}

// A replacing local move whose trash entry cannot be reserved fails: the
// reservation's not-exist is not read as a destination that left on its own,
// to be superseded when the move's rename finds the name taken. Both files
// stay where they are.
//
// Validates: R-6.4.9, R-2.17.9
func TestExecutor_LocalMove_AReservationThatFailsFailsTheReplacingMove(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetTrash(reservationLosingItsFolder(t))
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "a.txt", "moving\n")
	writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
	planned := plannedDestination(t, e, syncRoot, "b.txt")

	o := e.ExecuteMove(t.Context(), &Action{
		Type: ActionLocalMove, Path: "b.txt", OldPath: "a.txt", ReplaceLocalTarget: true,
		View: &PathView{
			Path: "b.txt", Local: planned,
			Baseline: &BaselineEntry{Path: "a.txt", ItemType: ItemTypeFile},
		},
	})

	requireOutcomeFailure(t, &o)
	require.NotErrorIs(t, o.Error, ErrActionPreconditionChanged)
	require.NotErrorIs(t, o.Error, os.ErrNotExist)
	require.ErrorContains(t, o.Error, "reserving a trash entry")
	requireExecFileContent(t, syncRoot, "b.txt", "bravo\n")
	requireExecFileContent(t, syncRoot, "a.txt", "moving\n")
}

// A run that died after setting a file aside leaves it under a hidden name in
// the sync folder. It is user content: every full scan reports it, nothing
// uploads it, and housekeeping never deletes it.
//
// Validates: R-6.4.9
func TestFullScan_LeftoverSetAsideFileIsReportedNotDeleted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	leftover := filepath.Join(syncRoot, ".onedrive-go.0123456789abcdef.detach")
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	runScenarioPass(t, eng)

	assert.FileExists(t, leftover, "an interrupted removal is never cleaned up automatically")
	assert.NotContains(t, remote.tree(), ".onedrive-go.0123456789abcdef.detach", "it is never uploaded")
	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	found := false
	for i := range issues {
		if issues[i].Path == ".onedrive-go.0123456789abcdef.detach" && issues[i].IssueType == IssueLocalInterruptedRemoval {
			found = true
		}
	}
	assert.True(t, found, "the leftover is reported as a durable issue naming its path: %+v", issues)
}

// A download may replace a local file only when that file is still the
// synced version. The plan's local hash is checked when it has one; when it
// has none, the baseline's local hash stands in, so an edit that kept size and
// mtime (a `cp -p` or `touch -r` edit) is never overwritten (D-066).
//
// Validates: R-2.2.5
func TestExecutor_Download_LocalContentDiffersFromBaselineIsSuperseded(t *testing.T) {
	t.Parallel()

	const (
		synced = "synced version"
		edited = "EDITED version"
		remote = "remote version"
	)
	for _, tt := range []struct {
		name        string
		withTrash   bool
		plannedHash bool
	}{
		{name: "planned hash, no trash", plannedHash: true},
		{name: "planned hash, trash", plannedHash: true, withTrash: true},
		{name: "no planned hash, no trash"},
		{name: "no planned hash, trash", withTrash: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dl := &executorMockDownloader{
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					n, err := w.Write([]byte(remote))
					return int64(n), err
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
			if tt.withTrash {
				cfg.SetTrash(localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil))
			}
			target := writeExecTestFile(t, syncRoot, "doc.txt", edited)
			info, err := os.Stat(target)
			require.NoError(t, err)

			syncedHash := testQuickXor(synced)
			planned := &LocalState{ItemType: ItemTypeFile, Size: info.Size(), Mtime: info.ModTime().UnixNano()}
			if tt.plannedHash {
				// The scan reused the stale baseline hash for the edited file.
				planned.Hash = syncedHash
			}
			action := &Action{
				Type:    ActionDownload,
				Path:    "doc.txt",
				ItemID:  "item-doc",
				DriveID: driveid.New(synctest.TestDriveID),
				View: &PathView{
					Local: planned,
					Remote: &RemoteState{
						ItemID: "item-doc", ItemType: ItemTypeFile, Hash: testQuickXor(remote),
						Size: int64(len(remote)), Mtime: 1,
					},
					Baseline: &BaselineEntry{
						Path: "doc.txt", ItemID: "item-doc", ItemType: ItemTypeFile,
						LocalHash: syncedHash, LocalSize: info.Size(), LocalSizeKnown: true,
						LocalMtime: info.ModTime().UnixNano(),
					},
				},
			}

			o := NewExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), action)

			requireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, ErrActionPreconditionChanged)
			data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
			require.NoError(t, err)
			assert.Equal(t, edited, string(data), "the unsynced local edit survives")
		})
	}
}

// Validates: R-2.17.9
//
// A replacing local move checks its source before it touches the destination.
// A source that is no longer the synced item supersedes the move, and the
// verified file at the destination stays where it is instead of having been
// moved to the trash first for a move that then never ran.
func TestExecutor_LocalMove_StaleSourceLeavesTheReplacedDestinationInPlace(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
	cfg.SetTrash(trash)
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "a.txt", "alpha\n")
	writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
	plannedHash, err := e.hashFunc(t.Context(), filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	source, err := e.syncTree.IdentityNoFollow("a.txt")
	require.NoError(t, err)

	action := &Action{
		Type:               ActionLocalMove,
		Path:               "b.txt",
		OldPath:            "a.txt",
		ReplaceLocalTarget: true,
		View: &PathView{
			Path:  "b.txt",
			Local: &LocalState{ItemType: ItemTypeFile, Hash: plannedHash, Size: info.Size()},
			// The synced item had another inode: a.txt is not it any more.
			Baseline: &BaselineEntry{
				Path: "a.txt", ItemType: ItemTypeFile,
				LocalDevice: source.Device, LocalInode: source.Inode + 1, LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, ErrActionPreconditionChanged)
	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "bravo\n", string(data), "the destination is untouched")
	entries, err := trash.List()
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing went to the trash for a move that did not run")
}
