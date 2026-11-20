package sync

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// A synced folder, with a subfolder, that ends up replaced by a file of the
// same content on both sides: the subtree is gone everywhere and the file at
// the folder's path is equal on the two sides, so nothing transfers and the
// baseline update has to record the file.
const (
	replacedFolderPath      = "notes"
	replacedFolderChildName = "drafts"
	replacingFileContent    = "notes, now one file\n"
)

// A publication records the item its observed sides hold. The type is the
// observed one, not the one the baseline recorded before the type changed.
// A hash the view left unknown comes from the baseline only when it holds the
// same type: a file keeps the hash OneDrive omits for it, and a folder has no
// hash, whatever the file before it had. An observed eTag stands even when it
// is empty, since the comparison reads it from current truth.
//
// Validates: R-2.17.4
func TestPublicationMutation_RecordsTheObservedItem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		baseline syncdomain.ItemType
		observed syncdomain.ItemType
		// The observed sides' hashes and the remote's eTag; empty is unknown.
		localHash, remoteHash, remoteETag string
		wantLocalHash, wantRemoteHash     string
	}{
		{
			name: "folder replaced by a file", baseline: syncdomain.ItemTypeFolder, observed: syncdomain.ItemTypeFile,
			localHash: "hash-after", remoteHash: "hash-after", remoteETag: "etag-after",
			wantLocalHash: "hash-after", wantRemoteHash: "hash-after",
		},
		{
			name: "file replaced by a folder", baseline: syncdomain.ItemTypeFile, observed: syncdomain.ItemTypeFolder,
			remoteETag: "etag-after",
		},
		{
			name: "file whose hash OneDrive omits", baseline: syncdomain.ItemTypeFile, observed: syncdomain.ItemTypeFile,
			localHash: "hash-before", remoteETag: "etag-after",
			wantLocalHash: "hash-before", wantRemoteHash: "hash-before",
		},
		{
			name: "file observed without an eTag", baseline: syncdomain.ItemTypeFile, observed: syncdomain.ItemTypeFile,
			localHash: "hash-after", remoteHash: "hash-after",
			wantLocalHash: "hash-after", wantRemoteHash: "hash-after",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			action := &syncdomain.Action{
				Type:    syncdomain.ActionBaselineUpdate,
				Path:    replacedFolderPath,
				ItemID:  "item-before",
				DriveID: driveid.New(syncfixture.EngineTestDriveID),
				View: &syncdomain.PathView{
					Baseline: &syncdomain.BaselineEntry{
						ItemID: "item-before", ItemType: tc.baseline,
						LocalHash: "hash-before", RemoteHash: "hash-before", ETag: "etag-before",
					},
					Remote: &syncdomain.RemoteState{ItemID: "item-after", ItemType: tc.observed, Hash: tc.remoteHash, ETag: tc.remoteETag},
					Local:  &syncdomain.LocalState{ItemType: tc.observed, Hash: tc.localHash},
				},
			}

			mutation, err := syncstore.PublicationMutationFromAction(action, driveid.New(syncfixture.EngineTestDriveID))
			require.NoError(t, err)
			assert.Equal(t, tc.observed, mutation.ItemType, "the observed sides decide the type")
			assert.Equal(t, "item-after", mutation.ItemID)
			assert.Equal(t, tc.wantLocalHash, mutation.LocalHash, "the local content")
			assert.Equal(t, tc.wantRemoteHash, mutation.RemoteHash, "the remote content")
			assert.Equal(t, tc.remoteETag, mutation.ETag, "the observed eTag, even an empty one")
		})
	}
}

// Both sides replace a synced folder with the same file. The first pass
// transfers nothing and records the file; the second plans nothing.
//
// Validates: R-2.17.4
func TestRunOnce_AFolderBothSidesReplacedWithTheSameFileConverges(t *testing.T) {
	t.Parallel()

	remote := remoteWithReplacedFolder()
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	remote.deleteAt(t, replacedFolderPath)
	remote.putFileAt(t, replacedFolderPath, replacingFileContent)
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, replacedFolderPath)))
	syncfixture.WriteLocalFile(t, syncRoot, replacedFolderPath, replacingFileContent)

	assertPublishesTheReplacingFileInOnePass(t, eng, remote)
}

// OneDrive replaces a synced folder with a file; the sync deletes the local
// folder and downloads the file, and the process stops right after the
// download's rename. After a plain crash the restart keeps the local deletes'
// outcomes, so no baseline row is left at the path. When the WAL is lost too,
// the restart opens the baseline of before the round: the folder, where both
// sides now hold the same file. Either way the first pass after the restart
// records the file and the second plans nothing.
//
// Validates: R-6.5.2, R-2.17.4
func TestCrashRecovery_AFolderReplacedOnOneDriveWhoseDownloadWasCutConverges(t *testing.T) {
	t.Parallel()

	for _, lostWAL := range []bool{false, true} {
		t.Run(crashCaseName(syncdomain.CrashPointDownloadRenamed, lostWAL), func(t *testing.T) {
			t.Parallel()

			recovered := crashAfterTheReplacingDownload(t, lostWAL)
			assertPublishesTheReplacingFileInOnePass(t, recovered.eng, recovered.remote)
			assert.Equal(t, replacingFileContent, harnessLocalTree(t, recovered.syncRoot)[replacedFolderPath])
			require.True(t, assertSyncInvariants(t, recovered), "state:%s", syncStateDump(t, recovered.eng))
		})
	}
}

// remoteWithReplacedFolder is a OneDrive holding the folder the scenarios
// replace and its subfolder.
func remoteWithReplacedFolder() *moveScenarioRemote {
	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-notes", "root", replacedFolderPath)
	remote.addFolder("item-drafts", "item-notes", replacedFolderChildName)

	return remote
}

// crashAfterTheReplacingDownload syncs the folder, replaces it on OneDrive
// with the file, and syncs with a capture armed at the download's rename. It
// returns the engine opened on what the capture kept, before any pass of its
// own.
func crashAfterTheReplacingDownload(t *testing.T, lostWAL bool) *syncHarness {
	t.Helper()

	remote := remoteWithReplacedFolder()
	h := newSyncHarness(t, remote)
	h.eng.serializeWorkers = true
	runScenarioPass(t, h.eng)
	// The synced folder reaches the main database file; the round's commits
	// stay in the WAL, which the lost-WAL variant then loses.
	_, err := h.eng.baseline.RawDBForTest().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)

	remote.deleteAt(t, replacedFolderPath)
	remote.putFileAt(t, replacedFolderPath, replacingFileContent)
	capture := &crashCapture{
		point: syncdomain.CrashPointDownloadRenamed, lostWAL: lostWAL,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.execCfg.SetCrashHookForTest(capture.hook)
	h.eng.baseline.SetCrashHookForTest(capture.hook)
	h.beginRound(t)
	report := runScenarioPass(t, h.eng)
	require.Equal(t, 1, report.Downloads, "the replacing file is downloaded")
	require.True(t, capture.captured, "the download reached %s", capture.point)

	recovered := capture.reopen(t)
	recovered.held = h.held

	return recovered
}

// assertPublishesTheReplacingFileInOnePass runs two passes: the first
// transfers nothing and publishes the replacing file, as a file with
// OneDrive's item ID and hash and without the folder's subtree, in the cached
// baseline and in its stored row; the second plans nothing.
func assertPublishesTheReplacingFileInOnePass(t *testing.T, eng *testEngine, remote *moveScenarioRemote) {
	t.Helper()

	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.BaselineUpdates, "the replacing file is published once")
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+
		report.FolderCreates+report.Moves+report.ConflictCopies, "nothing transfers: %+v", report)

	item, ok := remote.itemAt(replacedFolderPath)
	require.True(t, ok, "OneDrive holds the replacing file")
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath(replacedFolderPath)
	require.True(t, ok, "the replacing file has a baseline row")
	assert.Equal(t, syncdomain.ItemTypeFile, entry.ItemType, "the baseline records the file, not the folder it replaced")
	assert.Equal(t, item.ID, entry.ItemID)
	_, childKept := bl.GetByPath(path.Join(replacedFolderPath, replacedFolderChildName))
	assert.False(t, childKept, "the folder's subtree left the baseline")

	var stored struct {
		itemType   syncdomain.ItemType
		remoteHash string
	}
	require.NoError(t, eng.baseline.RawDBForTest().QueryRowContext(t.Context(),
		`SELECT item_type, COALESCE(remote_hash, '') FROM baseline WHERE path = ?`, replacedFolderPath).Scan(
		&stored.itemType, &stored.remoteHash))
	assert.Equal(t, syncdomain.ItemTypeFile, stored.itemType, "the stored row records the file")
	assert.Equal(t, item.QuickXorHash, stored.remoteHash, "the stored row holds the file's hash")

	assertPassPlannedNothing(t, runScenarioPass(t, eng))
}
