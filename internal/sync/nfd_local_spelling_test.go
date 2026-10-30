package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Scenarios on the real filesystem with names whose bytes are in Unicode NFD
// (R-2.13.2). Every stored path is NFC (R-2.13.1); on a byte-exact filesystem
// (Linux ext4, the CI runners) opening the NFC spelling of an NFD-named entry
// fails with ENOENT, so every operation on an existing entry has to find the
// spelling on disk. The emulated case-insensitive volume (internal/synctest)
// folds the two forms like macOS does and could not catch this.

// darwinGOOS is runtime.GOOS on macOS.
const darwinGOOS = "darwin"

const (
	// nfdCafe is "café" spelled as e + U+0301 COMBINING ACUTE ACCENT.
	nfdCafe = "café"
	// nfcCafe is the same name, precomposed (U+00E9).
	nfcCafe = "café"
)

// skipUnlessByteExactNames skips on macOS, whose volumes resolve either
// spelling to the same entry, so the scenario's premise does not hold.
func skipUnlessByteExactNames(t *testing.T) {
	t.Helper()

	if runtime.GOOS == darwinGOOS {
		t.Skip("macOS volumes are normalization-insensitive; both spellings name one entry")
	}
}

// localNames lists the raw names in dir, so a test sees the spelling on disk.
func localNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// syncedNFDFileHarness syncs a local corpus/<NFD café> nfd.txt to an empty
// fake OneDrive and returns the harness once the upload has converged.
func syncedNFDFileHarness(t *testing.T) *syncHarness {
	t.Helper()
	skipUnlessByteExactNames(t)

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	h := newSyncHarness(t, remote)
	writeLocalFile(t, h.syncRoot, filepath.Join("corpus", nfdCafe+" nfd.txt"), "nfd content\n")
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)
	require.Equal(t, 1, report.Uploads, "the NFD-named file uploads in one pass")

	return h
}

// Validates: R-2.13.1, R-2.13.2
//
// An NFD-named local file uploads in the first pass, under its NFC name, and
// the next pass is quiet. The upload used to be superseded on every pass
// ("upload source ... is missing") while the sync reported success.
func TestRunOnce_LocalNFDNamedFileUploadsInOnePass(t *testing.T) {
	t.Parallel()

	h := syncedNFDFileHarness(t)

	assert.Equal(t, map[string]string{
		"corpus":                         modelFolderSentinel,
		"corpus/" + nfcCafe + " nfd.txt": "nfd content\n",
	}, h.remote.tree())
	assert.Equal(t, []string{nfdCafe + " nfd.txt"}, localNames(t, filepath.Join(h.syncRoot, "corpus")),
		"the local file keeps its own spelling")
	assert.True(t, assertQuietNextPass(t, h.eng))
}

// nfcLocalTree is the sync root as path -> content with every name in NFC,
// the form OneDrive's tree is compared in.
func nfcLocalTree(t *testing.T, syncRoot string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for p, content := range harnessLocalTree(t, syncRoot) {
		out[nfcNormalize(p)] = content
	}

	return out
}

// assertNFDConverged is assertSyncInvariants for a tree holding NFD names:
// the trees are compared in NFC, and nothing is lost.
func assertNFDConverged(t *testing.T, h *syncHarness) {
	t.Helper()

	assert.True(t, assertQuietNextPass(t, h.eng))
	assert.Equal(t, h.remote.tree(), nfcLocalTree(t, h.syncRoot), "local and OneDrive trees converge")
	assertNothingLost(t, h, harnessLocalTree(t, h.syncRoot))
	assertStoreIntegrity(t, h.eng)
}

// Validates: R-2.13.2
//
// A remote edit of a file that is NFD-named on disk replaces that file, in
// place, keeping its spelling. Writing the stored NFC spelling would leave a
// second file beside it.
func TestRunOnce_RemoteEditReplacesTheNFDNamedLocalFile(t *testing.T) {
	t.Parallel()

	h := syncedNFDFileHarness(t)
	h.remote.putFileAt(t, "corpus/"+nfcCafe+" nfd.txt", "edited online\n")
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, []string{nfdCafe + " nfd.txt"}, localNames(t, filepath.Join(h.syncRoot, "corpus")),
		"the download replaced the NFD-named file instead of writing an NFC sibling")
	content, err := os.ReadFile(filepath.Join(h.syncRoot, "corpus", nfdCafe+" nfd.txt"))
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(content))
	assertNFDConverged(t, h)
}

// Validates: R-2.13.2
//
// A remote delete removes the NFD-named local file.
func TestRunOnce_RemoteDeleteRemovesTheNFDNamedLocalFile(t *testing.T) {
	t.Parallel()

	h := syncedNFDFileHarness(t)
	h.remote.deleteAt(t, "corpus/"+nfcCafe+" nfd.txt")
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.LocalDeletes)
	assert.Empty(t, localNames(t, filepath.Join(h.syncRoot, "corpus")))
	assertNFDConverged(t, h)
}

// Validates: R-2.13.2
//
// A remote rename, and a remote move to another folder, move the NFD-named
// local file. A moved file is a new entry at its destination and takes the
// stored spelling there.
func TestRunOnce_RemoteMoveMovesTheNFDNamedLocalFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		to   string
		// left is what corpus holds after the move.
		left []string
	}{
		{name: "rename", to: "corpus/renamed.txt", left: []string{"renamed.txt"}},
		{name: "other folder", to: "other/" + nfcCafe + " nfd.txt", left: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := syncedNFDFileHarness(t)
			require.NoError(t, os.Mkdir(filepath.Join(h.syncRoot, "other"), 0o750))
			runScenarioPass(t, h.eng)

			h.remote.moveAt(t, "corpus/"+nfcCafe+" nfd.txt", tc.to)
			h.beginRound(t)
			report := runScenarioPass(t, h.eng)

			assert.Equal(t, 1, report.Moves)
			assert.Equal(t, tc.left, localNames(t, filepath.Join(h.syncRoot, "corpus")), "the source left")
			content, err := os.ReadFile(filepath.Join(h.syncRoot, filepath.FromSlash(tc.to)))
			require.NoError(t, err)
			assert.Equal(t, "nfd content\n", string(content))
			assertNFDConverged(t, h)
		})
	}
}

// Validates: R-2.13.2
//
// An NFD-named local folder syncs, receives remote additions inside itself
// (a file and a subfolder, not an NFC-named twin folder), and is removed
// when it is deleted online.
func TestRunOnce_NFDNamedLocalFolderReceivesAdditionsAndIsDeleted(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	h := newSyncHarness(t, remote)
	writeLocalFile(t, h.syncRoot, filepath.Join(nfdCafe+" dir", "inside.txt"), "inside\n")
	// A file outside the folder keeps the folder delete below from emptying
	// the drive, which the delete-safety hold would hold for confirmation.
	writeLocalFile(t, h.syncRoot, "keep.txt", "keep\n")
	h.beginRound(t)
	runScenarioPass(t, h.eng)
	require.Equal(t, map[string]string{
		nfcCafe + " dir":            modelFolderSentinel,
		nfcCafe + " dir/inside.txt": "inside\n",
		"keep.txt":                  "keep\n",
	}, remote.tree())

	remote.putFileAt(t, nfcCafe+" dir/new.txt", "new online\n")
	remote.mkdirAt(t, nfcCafe+" dir/sub")
	h.beginRound(t)
	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, 1, report.FolderCreates)
	assert.Equal(t, []string{nfdCafe + " dir", "keep.txt"}, localNames(t, h.syncRoot), "no NFC-named twin folder")
	assert.ElementsMatch(t, []string{"inside.txt", "new.txt", "sub"}, localNames(t, filepath.Join(h.syncRoot, nfdCafe+" dir")))
	assertNFDConverged(t, h)

	remote.deleteAt(t, nfcCafe+" dir")
	h.beginRound(t)
	runScenarioPass(t, h.eng)

	assert.Equal(t, []string{"keep.txt"}, localNames(t, h.syncRoot), "the folder delete removed the NFD-named folder")
	assertNFDConverged(t, h)
}

// Validates: R-2.13.2
//
// Edits on both sides of an NFD-named file keep both versions: the conflict
// copy is made from the file on disk, not reported done because the stored
// spelling was missing.
func TestRunOnce_ConflictingEditsOfAnNFDNamedFileKeepBoth(t *testing.T) {
	t.Parallel()

	h := syncedNFDFileHarness(t)
	h.eng.clock = &modelClock{}
	writeLocalFile(t, h.syncRoot, filepath.Join("corpus", nfdCafe+" nfd.txt"), "edited here\n")
	h.remote.putFileAt(t, "corpus/"+nfcCafe+" nfd.txt", "edited online\n")
	h.beginRound(t)

	for range 3 {
		_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}

	local := nfcLocalTree(t, h.syncRoot)
	assert.Contains(t, modelContents(local), "edited here\n")
	assert.Contains(t, modelContents(local), "edited online\n")
	assertNFDConverged(t, h)
}

// Validates: R-2.13.2
//
// Watch-mode re-checks look an NFD-named file up by its spelling on disk: a
// delete event for it while it still exists (an atomic save) is not a
// delete, and its synced path is not gone.
func TestWatchRechecks_FindTheNFDNamedLocalFile(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	syncRoot := t.TempDir()
	writeLocalFile(t, syncRoot, filepath.Join(nfdCafe+" dir", nfdCafe+".txt"), "x")
	tree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	stored := nfcCafe + " dir/" + nfcCafe + ".txt"
	fsPath := filepath.Join(syncRoot, nfdCafe+" dir", nfdCafe+".txt")

	assert.True(t, localPathStillPresent(tree, stored, fsPath), "the event's file still exists")
	assert.False(t, syncedPathGone(tree, stored), "the synced file is still there")

	require.NoError(t, os.Remove(fsPath))
	assert.False(t, localPathStillPresent(tree, stored, fsPath))
	assert.True(t, syncedPathGone(tree, stored))
}

// Validates: R-2.13.2, R-2.8.13
//
// A full scan's rows carry the stored NFC path; the watch on an NFD-named
// folder is added on its spelling on disk. Adding the NFC spelling failed
// with ENOENT on every scan and left the folder unwatched.
func TestRewatchUnwatchedDirectories_WatchesTheNFDNamedFolder(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	dir := t.TempDir()
	onDisk := filepath.Join(dir, nfdCafe+" dir")
	require.NoError(t, os.Mkdir(onDisk, 0o700))
	watcher := newAddErrWatcher(nil, nil)
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.activeWatcher = watcher

	obs.rewatchUnwatchedDirectories(mustOpenSyncTree(t, dir), []localStateRow{{Path: nfcCafe + " dir", ItemType: ItemTypeFolder}})

	assert.Contains(t, watcher.addedPaths(), onDisk)
	assert.NotContains(t, watcher.addedPaths(), filepath.Join(dir, nfcCafe+" dir"))
}

// Validates: R-2.13.2, R-2.10.61
//
// The local permission probe inspects an NFD-named folder by its spelling on
// disk and reports the boundary in the stored NFC form. Probing the NFC
// spelling found a "denied" folder that was only absent under that spelling.
func TestPermHandler_HandleLocalPermission_ProbesTheNFDNamedFolder(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	t.Run("accessible folder keeps the denial per file", func(t *testing.T) {
		t.Parallel()

		ph, syncRoot := newTestPermHandler(t, nil)
		require.NoError(t, os.Mkdir(filepath.Join(syncRoot, nfdCafe), 0o750))

		evidence := ph.handleLocalPermission(t.Context(), &actionCompletion{
			Path:       nfcCafe + "/file.txt",
			ActionType: ActionDownload,
			ErrMsg:     "permission denied",
		})

		assert.Equal(t, permissionEvidenceFileDenied, evidence.Kind)
		assert.Equal(t, nfcCafe+"/file.txt", evidence.TriggerPath)
	})

	t.Run("read-only mount is one stored boundary", func(t *testing.T) {
		t.Parallel()

		ph, syncRoot := newTestPermHandler(t, nil)
		mount := filepath.Join(syncRoot, nfdCafe)
		require.NoError(t, os.MkdirAll(filepath.Join(mount, "photos"), 0o750))
		ph.readOnlyDir = func(absDir string) bool {
			return absDir == mount || strings.HasPrefix(absDir, mount+string(filepath.Separator))
		}

		evidence := ph.handleLocalPermission(t.Context(), &actionCompletion{
			Path:       nfcCafe + "/photos/a.jpg",
			ActionType: ActionDownload,
			Err:        &os.PathError{Op: "open", Path: "a.jpg", Err: syscall.EROFS},
		})

		assert.Equal(t, permissionEvidenceBoundaryDenied, evidence.Kind)
		assert.Equal(t, nfcCafe, evidence.BoundaryPath)
	})
}

// Validates: R-2.13.2, R-2.8.10
//
// A remote delete planned from local absence is superseded when the file is
// back locally under its NFD spelling: absence is judged on disk, not by the
// stored NFC spelling alone.
func TestExecuteRemoteDelete_NFDNamedLocalFileBackIsSuperseded(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	driveID := driveid.New(synctest.TestDriveID)
	stored := nfcCafe + ".txt"
	var deleted []string
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: stored, ParentID: "root", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			deleted = append(deleted, itemID)
			return nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	writeExecTestFile(t, syncRoot, nfdCafe+".txt", "back again\n")

	action := &Action{
		Type: ActionRemoteDelete, Path: stored, ItemID: "item-a", DriveID: driveID,
		View: &pathView{
			Path:   stored,
			Remote: &remoteState{DriveID: driveID, ItemID: "item-a", ItemType: ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
		},
	}
	outcome := newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
	assert.Empty(t, deleted, "a file that is back locally must not be deleted on OneDrive")
}
