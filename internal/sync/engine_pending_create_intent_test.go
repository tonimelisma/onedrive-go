package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-5.6.15
//
// Live, 2026-11-01: the watch client was killed with uploads in flight.
// OneDrive created one file's empty item two seconds after the kill, so the
// record of it was never written, and the next pass planned a create
// conflict: the user's file was renamed to a conflict copy and the empty
// item downloaded in its place. The intent recorded before the request is
// what the recovered engine recognizes the empty item by.
func TestRunOnce_EmptyItemACrashLeftUnrecordedIsFinishedNotConflicted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "orphan", "root", "doc.txt", "")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.NoError(t, eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveid.New(engineTestDriveID), Path: "doc.txt", ParentID: "root",
	}))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("the user's content\n"), 0o600))

	report := runScenarioPass(t, eng)

	assert.Equal(t, "the user's content\n", remote.tree()["doc.txt"], "the content goes into the empty item")
	assert.Equal(t, 1, report.Uploads)
	assert.Zero(t, report.Downloads, "the empty item is not downloaded over the user's file")
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".conflict-", "no conflict copy")
	}
	assertSyncInvariants(t, &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot})
}

// Validates: R-5.6.15
//
// A create ended without an answer before it reached OneDrive (a connection
// reset), so its intent was kept for a create that might still land. The
// user then renamed the file away: no create at the old path is retried, no
// item appears there, and the intent stayed for good (model test, chaos seeds
// 9 and 11). The next pass, whose complete local scan finds nothing at the
// path, drops it.
func TestRunOnce_CreateIntentOfAFileRenamedAwayIsDropped(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	original := filepath.Join(h.syncRoot, "doc.txt")
	require.NoError(t, os.WriteFile(original, []byte("my words\n"), 0o600))

	refuseCreatesUnanswered(remote, true)
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Empty(t, remote.tree(), "the create never reached OneDrive")
	pending, found, err := h.eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the intent outlives a create whose outcome is unknown")
	require.Empty(t, pending.ItemID)

	refuseCreatesUnanswered(remote, false)
	require.NoError(t, os.Rename(original, filepath.Join(h.syncRoot, "renamed.txt")))
	expireRetryWork(t, h.eng)
	h.beginRound(t)
	runScenarioPass(t, h.eng)

	_, found, err = h.eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found, "no create will ever claim the intent of a file renamed away")
	assert.Equal(t, map[string]string{"renamed.txt": "my words\n"}, remote.tree())
	assertSyncInvariants(t, h)
}

// Validates: R-5.6.15
//
// The process died with a create in flight, OneDrive made the empty item
// after it was gone, and the user deleted the local file before the restart.
// The remote observation claims the empty file for the intent before the
// step after planning drops intents whose file is gone, so by then the record
// names the item: the pass deletes the placeholder instead of downloading an
// empty file the user never had.
func TestRunOnce_EmptyItemOfAKilledCreateWhoseFileWasDeletedIsRemoved(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	// A file both sides keep, so deleting the placeholder does not empty the
	// remote side, which the delete-safety hold would hold for confirmation.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	require.NoError(t, h.eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveid.New(engineTestDriveID), Path: "doc.txt", ParentID: "root",
	}))
	remote.mu.Lock()
	remote.addFile(t, "orphan", "root", "doc.txt", "")
	remote.mu.Unlock()
	h.beginRound(t)
	report := runScenarioPass(t, h.eng)

	assert.Equal(t, map[string]string{"keep.txt": "keep\n"}, remote.tree(), "the empty placeholder is deleted")
	assert.Zero(t, report.Downloads, "the placeholder is not downloaded")
	assert.NoFileExists(t, filepath.Join(h.syncRoot, "doc.txt"))
	assertSyncInvariants(t, h)
}

// Validates: R-5.6.15
//
// A watch replan refreshes the local tree and plans on the OneDrive changes
// the observers committed before it. A create whose answer was lost may have
// landed without a change batch showing it yet, so a replan after the user
// removed the local file keeps the fresh intent: dropped there, the batch
// that then showed the empty file would have it downloaded as a stranger's.
// Kept, the batch claims it, and the next replan deletes the placeholder.
func TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	// A file both sides keep, so deleting the placeholder does not empty the
	// remote side, which the delete-safety hold would hold for confirmation.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	ctx := t.Context()
	require.NoError(t, eng.baseline.RecordPendingCreateIntent(ctx, &PendingCreate{
		DriveID: driveID, Path: "doc.txt", ParentID: "root",
	}))

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	pipe := &watchPipeline{bl: bl, mode: SyncBidirectional}
	applied, err := rt.runSteadyStateReplan(ctx, pipe, dirtyBatch{})
	require.NoError(t, err)
	require.True(t, applied)
	pending, found, err := eng.baseline.PendingCreateAt(ctx, "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "a replan that observed only the local tree keeps a fresh intent")
	require.Empty(t, pending.ItemID)

	// The create had landed: the next change batch shows its empty file.
	remote.mu.Lock()
	remote.addFile(t, "orphan", "root", "doc.txt", "")
	landed := *remote.items["orphan"]
	remote.mu.Unlock()
	require.NoError(t, rt.handleRemoteObservationBatch(ctx, &remoteObservationBatch{
		source: remoteObservationBatchPrimaryWatch,
		observed: []observedItem{{
			DriveID: driveID, ItemID: landed.ID, ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
			Hash: landed.QuickXorHash, Size: 0, Mtime: landed.ModifiedAt.UnixNano(), ETag: landed.ETag,
		}},
	}))
	pending, found, err = eng.baseline.PendingCreateAt(ctx, "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, landed.ID, pending.ItemID, "the batch claims the empty file for the intent")

	applied, err = rt.runSteadyStateReplan(ctx, pipe, dirtyBatch{})
	require.NoError(t, err)
	require.True(t, applied)
	outbox := rt.currentOutbox()
	require.Len(t, outbox, 1, "one action: the placeholder's delete")
	assert.Equal(t, ActionRemoteDelete, outbox[0].Action.Type, "not a download of the empty file")
	assert.Equal(t, "doc.txt", outbox[0].Action.Path)
	assert.Equal(t, landed.ID, outbox[0].Action.ItemID)

	// The pass that runs it deletes the placeholder and downloads nothing.
	report := runScenarioPass(t, eng)
	assert.Equal(t, map[string]string{"keep.txt": "keep\n"}, remote.tree(), "the empty placeholder is deleted")
	assert.Zero(t, report.Downloads)
	assert.NoFileExists(t, filepath.Join(syncRoot, "doc.txt"))
}

// refuseCreatesUnanswered makes every create of remote fail the way a
// connection reset before the request reached OneDrive does: an unknown
// outcome, and nothing made.
func refuseCreatesUnanswered(remote *moveScenarioRemote, refuse bool) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if !refuse {
		remote.refuseCreate = nil
		return
	}
	remote.refuseCreate = func(_, name string) error { return scenarioCreateAnswerLost(name) }
}
