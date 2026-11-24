package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Scenarios on the real filesystem with names whose bytes are in Unicode NFD
// (R-2.13.2). Every stored path is NFC (R-2.13.1); on a byte-exact filesystem
// (Linux ext4, the CI runners) opening the NFC spelling of an NFD-named entry
// fails with ENOENT, so every operation on an existing entry has to find the
// spelling on disk. The emulated case-insensitive volume (internal/synctest)
// folds the two forms like macOS does and could not catch this.

// syncedNFDFileHarness syncs a local corpus/<NFD café> nfd.txt to an empty
// fake OneDrive and returns the harness once the upload has converged.
func syncedNFDFileHarness(t *testing.T) *syncHarness {
	t.Helper()
	syncfixture.SkipUnlessByteExactNames(t)

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	h := newSyncHarness(t, remote)
	syncfixture.WriteLocalFile(t, h.syncRoot, filepath.Join("corpus", syncfixture.NfdCafe+" nfd.txt"), "nfd content\n")
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
		"corpus": syncfixture.ModelFolderSentinel,
		"corpus/" + syncfixture.NfcCafe + " nfd.txt": "nfd content\n",
	}, h.remote.tree())
	assert.Equal(t, []string{syncfixture.NfdCafe + " nfd.txt"}, syncfixture.LocalNames(t, filepath.Join(h.syncRoot, "corpus")),
		"the local file keeps its own spelling")
	assert.True(t, assertQuietNextPass(t, h.eng))
}

// nfcLocalTree is the sync root as path -> content with every name in NFC,
// the form OneDrive's tree is compared in.
func nfcLocalTree(t *testing.T, syncRoot string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for p, content := range harnessLocalTree(t, syncRoot) {
		out[namekey.StoredPath(p)] = content
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
	h.remote.putFileAt(t, "corpus/"+syncfixture.NfcCafe+" nfd.txt", "edited online\n")
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, []string{syncfixture.NfdCafe + " nfd.txt"}, syncfixture.LocalNames(t, filepath.Join(h.syncRoot, "corpus")),
		"the download replaced the NFD-named file instead of writing an NFC sibling")
	content, err := os.ReadFile(filepath.Join(h.syncRoot, "corpus", syncfixture.NfdCafe+" nfd.txt"))
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
	h.remote.deleteAt(t, "corpus/"+syncfixture.NfcCafe+" nfd.txt")
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.LocalDeletes)
	assert.Empty(t, syncfixture.LocalNames(t, filepath.Join(h.syncRoot, "corpus")))
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
		{name: "other folder", to: "other/" + syncfixture.NfcCafe + " nfd.txt", left: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := syncedNFDFileHarness(t)
			require.NoError(t, os.Mkdir(filepath.Join(h.syncRoot, "other"), 0o750))
			runScenarioPass(t, h.eng)

			h.remote.moveAt(t, "corpus/"+syncfixture.NfcCafe+" nfd.txt", tc.to)
			h.beginRound(t)
			report := runScenarioPass(t, h.eng)

			assert.Equal(t, 1, report.Moves)
			assert.Equal(t, tc.left, syncfixture.LocalNames(t, filepath.Join(h.syncRoot, "corpus")), "the source left")
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
	syncfixture.SkipUnlessByteExactNames(t)

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	h := newSyncHarness(t, remote)
	syncfixture.WriteLocalFile(t, h.syncRoot, filepath.Join(syncfixture.NfdCafe+" dir", "inside.txt"), "inside\n")
	// A file outside the folder keeps the folder delete below from emptying
	// the drive, which the delete-safety hold would hold for confirmation.
	syncfixture.WriteLocalFile(t, h.syncRoot, "keep.txt", "keep\n")
	h.beginRound(t)
	runScenarioPass(t, h.eng)
	require.Equal(t, map[string]string{
		syncfixture.NfcCafe + " dir":            syncfixture.ModelFolderSentinel,
		syncfixture.NfcCafe + " dir/inside.txt": "inside\n",
		"keep.txt":                              "keep\n",
	}, remote.tree())

	remote.putFileAt(t, syncfixture.NfcCafe+" dir/new.txt", "new online\n")
	remote.mkdirAt(t, syncfixture.NfcCafe+" dir/sub")
	h.beginRound(t)
	report := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, 1, report.FolderCreates)
	assert.Equal(t, []string{syncfixture.NfdCafe + " dir", "keep.txt"}, syncfixture.LocalNames(t, h.syncRoot), "no NFC-named twin folder")
	assert.ElementsMatch(t, []string{"inside.txt", "new.txt", "sub"}, syncfixture.LocalNames(t, filepath.Join(h.syncRoot, syncfixture.NfdCafe+" dir")))
	assertNFDConverged(t, h)

	remote.deleteAt(t, syncfixture.NfcCafe+" dir")
	h.beginRound(t)
	runScenarioPass(t, h.eng)

	assert.Equal(t, []string{"keep.txt"}, syncfixture.LocalNames(t, h.syncRoot), "the folder delete removed the NFD-named folder")
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
	h.eng.hooks.clock = newModelClock(1)
	syncfixture.WriteLocalFile(t, h.syncRoot, filepath.Join("corpus", syncfixture.NfdCafe+" nfd.txt"), "edited here\n")
	h.remote.putFileAt(t, "corpus/"+syncfixture.NfcCafe+" nfd.txt", "edited online\n")
	h.beginRound(t)

	for range 3 {
		_, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}

	local := nfcLocalTree(t, h.syncRoot)
	assert.Contains(t, modelContents(local), "edited here\n")
	assert.Contains(t, modelContents(local), "edited online\n")
	assertNFDConverged(t, h)
}

// Validates: R-2.13.2, R-2.10.61
//
// The local permission probe inspects an NFD-named folder by its spelling on
// disk and reports the boundary in the stored NFC form. Probing the NFC
// spelling found a "denied" folder that was only absent under that spelling.
func TestPermHandler_HandleLocalPermission_ProbesTheNFDNamedFolder(t *testing.T) {
	t.Parallel()
	syncfixture.SkipUnlessByteExactNames(t)

	t.Run("accessible folder keeps the denial per file", func(t *testing.T) {
		t.Parallel()

		ph, syncRoot := newTestPermHandler(t, nil)
		require.NoError(t, os.Mkdir(filepath.Join(syncRoot, syncfixture.NfdCafe), 0o750))

		evidence := ph.handleLocalPermission(t.Context(), &syncdomain.ActionCompletion{
			Path:       syncfixture.NfcCafe + "/file.txt",
			ActionType: syncdomain.ActionDownload,
			ErrMsg:     "permission denied",
		})

		assert.Equal(t, permissionEvidenceFileDenied, evidence.Kind)
		assert.Equal(t, syncfixture.NfcCafe+"/file.txt", evidence.TriggerPath)
	})

	t.Run("read-only mount is one stored boundary", func(t *testing.T) {
		t.Parallel()

		syncRoot := newTestPermSyncRoot(t)
		mount := filepath.Join(syncRoot, syncfixture.NfdCafe)
		require.NoError(t, os.MkdirAll(filepath.Join(mount, "photos"), 0o750))
		ph := newTestPermHandlerIn(t, nil, syncRoot, func(absDir string) bool {
			return absDir == mount || strings.HasPrefix(absDir, mount+string(filepath.Separator))
		})

		evidence := ph.handleLocalPermission(t.Context(), syncexec.CompletionForTest(&syncdomain.ActionCompletion{
			Path:       syncfixture.NfcCafe + "/photos/a.jpg",
			ActionType: syncdomain.ActionDownload,
			Err:        &os.PathError{Op: "open", Path: "a.jpg", Err: syscall.EROFS},
		}))

		assert.Equal(t, permissionEvidenceBoundaryDenied, evidence.Kind)
		assert.Equal(t, syncfixture.NfcCafe, evidence.BoundaryPath)
	})
}

// Validates: R-2.13.2, R-2.8.10
//
// A remote delete planned from local absence is superseded when the file is
// back locally under its NFD spelling: absence is judged on disk, not by the
// stored NFC spelling alone.
func TestExecuteRemoteDelete_NFDNamedLocalFileBackIsSuperseded(t *testing.T) {
	t.Parallel()
	syncfixture.SkipUnlessByteExactNames(t)

	driveID := driveid.New(synctest.TestDriveID)
	stored := syncfixture.NfcCafe + ".txt"
	var deleted []string
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: stored, ParentID: "root", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
		},
		DeleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			deleted = append(deleted, itemID)
			return nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	syncfixture.WriteExecTestFile(t, syncRoot, syncfixture.NfdCafe+".txt", "back again\n")

	action := &syncdomain.Action{
		Type: syncdomain.ActionRemoteDelete, Path: stored, ItemID: "item-a", DriveID: driveID,
		View: &syncdomain.PathView{
			Path:   stored,
			Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item-a", ItemType: syncdomain.ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
		},
	}
	outcome := syncexec.NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Empty(t, deleted, "a file that is back locally must not be deleted on OneDrive")
}

// Validates: R-2.13.2
//
// A second local file beside a synced one, whose name differs only in Unicode
// form, is a name collision in which neither file is chosen: no pass uploads
// the twin over the synced file, the collision is reported for the stored
// path, and once the twin is removed the synced file syncs as before.
func TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing(t *testing.T) {
	t.Parallel()
	syncfixture.SkipUnlessBothUnicodeFormsCoexist(t)

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	h := newSyncHarness(t, remote)
	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSyncedName, syncfixture.TwinSyncedContent)
	h.beginRound(t)
	require.Equal(t, 1, runScenarioPass(t, h.eng).Uploads)

	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSecondName, "twin content, longer\n")
	for pass := range 2 {
		report := runScenarioPass(t, h.eng)
		assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes,
			"pass %d acts on neither file: %+v", pass+1, report)
	}
	assert.Equal(t, map[string]string{syncfixture.TwinSyncedName: syncfixture.TwinSyncedContent}, remote.tree())

	issues, err := h.eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, syncfixture.TwinSyncedName, issues[0].Path)
	assert.Equal(t, syncdomain.IssueCaseCollision, issues[0].IssueType)

	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, syncfixture.TwinSecondName)))
	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSyncedName, "edited after the twin went\n")
	assert.Equal(t, 1, runScenarioPass(t, h.eng).Uploads)
	assert.Equal(t, map[string]string{syncfixture.TwinSyncedName: "edited after the twin went\n"}, remote.tree())
	issues, err = h.eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	assert.Empty(t, issues)
}

// syncedFileWithRemovedTwin syncs the synced file, records its Unicode-form
// twin as a collision, then removes the twin from disk and returns the
// harness. The twin's delete event is still to be handled.
func syncedFileWithRemovedTwin(t *testing.T) *syncHarness {
	t.Helper()
	syncfixture.SkipUnlessBothUnicodeFormsCoexist(t)

	h := newSyncHarness(t, newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID)))
	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSyncedName, syncfixture.TwinSyncedContent)
	require.Equal(t, 1, runScenarioPass(t, h.eng).Uploads)
	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSecondName, "twin content, longer\n")
	runScenarioPass(t, h.eng)
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, syncfixture.TwinSecondName)))

	return h
}

// handleTwinDelete runs the watch observer's delete of the removed twin and
// returns the observations it sent.
func handleTwinDelete(t *testing.T, h *syncHarness) []syncobserve.LocalObservation {
	t.Helper()

	bl, err := h.eng.baseline.Load(t.Context())
	require.NoError(t, err)
	batches := make(chan syncobserve.LocalObservation, 1)
	obs := syncobserve.NewLocalObserver(&syncobserve.LocalObserverInputs{Baseline: bl, Logger: h.eng.logger, Batches: batches})
	obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
	obs.HashRequests = make(chan syncobserve.HashRequest, syncobserve.HashRequestBufSize)

	obs.HandleDelete(t.Context(), syncfixture.NewRecordingFsWatcher(), h.eng.syncTree,
		filepath.Join(h.syncRoot, syncfixture.TwinSecondName), syncfixture.TwinSyncedName, syncfixture.TwinSyncedName)

	var sent []syncobserve.LocalObservation
	for len(batches) > 0 {
		sent = append(sent, <-batches)
	}

	return sent
}

// Validates: R-2.8.8, R-2.13.2
//
// Removing the Unicode-form twin of a synced file is no delete of the synced
// file, whose stored path the twin shared: the watch observer sends nothing,
// and the synced file's row stays.
func TestHandleDelete_UnicodeFormTwinKeepsTheSyncedFilesRow(t *testing.T) {
	t.Parallel()

	h := syncedFileWithRemovedTwin(t)

	assert.Empty(t, handleTwinDelete(t, h), "the stored path still names the synced file")
	_, found, err := h.eng.baseline.GetLocalStateByPath(t.Context(), syncfixture.TwinSyncedName)
	require.NoError(t, err)
	assert.True(t, found)
}

// Validates: R-2.13.2
func TestRunOnce_UnicodeFormFolderTwinOfSyncedFileFailsNothing(t *testing.T) {
	t.Parallel()
	syncfixture.SkipUnlessBothUnicodeFormsCoexist(t)

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	h := newSyncHarness(t, remote)
	syncfixture.WriteLocalFile(t, h.syncRoot, syncfixture.TwinSyncedName, syncfixture.TwinSyncedContent)
	require.Equal(t, 1, runScenarioPass(t, h.eng).Uploads)

	syncfixture.WriteLocalFile(t, h.syncRoot, filepath.Join(syncfixture.TwinSecondName, "child.txt"), "child\n")
	for pass := range 2 {
		report := runScenarioPass(t, h.eng)
		assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes,
			"pass %d acts on neither entry: %+v", pass+1, report)
	}
	assert.Equal(t, map[string]string{syncfixture.TwinSyncedName: syncfixture.TwinSyncedContent}, remote.tree())

	issues, err := h.eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, syncdomain.IssueCaseCollision, issues[0].IssueType)
}
