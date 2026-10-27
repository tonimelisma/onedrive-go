package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// seedDeleteSafetyRemote builds a remote with count files spread over two
// folders plus one loose file that no test deletes, so the local side never
// becomes structurally empty unless a test empties it on purpose.
func seedDeleteSafetyRemote(t *testing.T, count int) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-photos", "root", "Photos")
	remote.addFolder("item-docs", "root", "Docs")
	for i := range count {
		parent := "item-photos"
		if i%2 == 1 {
			parent = "item-docs"
		}
		remote.addFile(t, fmt.Sprintf("item-f%03d", i), parent, fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("file %d\n", i))
	}
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	return remote
}

func deleteSafetyFilePath(syncRoot string, i int) string {
	folder := "Photos"
	if i%2 == 1 {
		folder = "Docs"
	}
	return filepath.Join(syncRoot, folder, fmt.Sprintf("f%03d.txt", i))
}

func readHeldDeletes(t *testing.T, eng *testEngine) []HeldDeleteRow {
	t.Helper()

	rows, err := queryHeldDeleteRowsWithRunner(t.Context(), eng.baseline.rawDB())
	require.NoError(t, err)
	return rows
}

// Validates: R-6.2.5, R-6.4.2
func TestPlanner_MassRemoteDeleteIsHeldForConfirmation(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 100)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// 60 of 100 synced files vanish locally: above max(50, 30% of 100).
	for i := range 60 {
		require.NoError(t, os.Remove(deleteSafetyFilePath(syncRoot, i)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "new.txt"), []byte("new\n"), 0o600))

	report := runScenarioPass(t, eng)

	assert.Empty(t, remote.deletes, "the deletes must wait for confirmation")
	assert.Equal(t, 1, remote.uploads, "non-delete work keeps running while deletes are held")
	assert.Equal(t, 60, report.HeldRemoteDeletes)
	assert.Zero(t, report.HeldLocalDeletes)

	held := readHeldDeletes(t, eng)
	require.Len(t, held, 60, "status reads the held deletes from the store")
	assert.Equal(t, "remote", held[0].Side)
	assert.Equal(t, string(deleteSafetyReasonTooMany), held[0].Reason)

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	groups := ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1)
	assert.Equal(t, ConditionDeletesHeld, groups[0].ConditionKey)
	assert.Equal(t, 60, groups[0].Count)

	// The hold is stable while truth is unchanged.
	report = runScenarioPass(t, eng)
	assert.Empty(t, remote.deletes)
	assert.Equal(t, 60, report.HeldRemoteDeletes)

	// `sync --confirm-deletes` applies them and clears the condition.
	report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{ConfirmDeletes: true})
	require.NoError(t, err)
	require.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Len(t, remote.deletes, 60)
	assert.Zero(t, report.HeldRemoteDeletes)
	assert.Empty(t, readHeldDeletes(t, eng))
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-6.2.5, R-6.4.2
func TestPlanner_MassLocalDeleteAfterAnomalousFullRefreshIsHeld(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 3)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, deleteSafetyFilePath(syncRoot, 0))

	// A complete enumeration that returns only the root: the remote side
	// vanished. Even three deletes are held, because no rule about counts
	// can tell this from a person deleting everything.
	remote.mu.Lock()
	for id := range remote.items {
		if id != strRoot {
			delete(remote.items, id)
		}
	}
	remote.mu.Unlock()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)

	assert.FileExists(t, deleteSafetyFilePath(syncRoot, 0), "local files must survive an empty remote enumeration")
	assert.FileExists(t, filepath.Join(syncRoot, "keep.txt"))
	assert.Equal(t, 6, report.HeldLocalDeletes, "every local delete is held: four files and two folders")
	held := readHeldDeletes(t, eng)
	require.NotEmpty(t, held)
	assert.Equal(t, "local", held[0].Side)
	assert.Equal(t, string(deleteSafetyReasonSideVanished), held[0].Reason)
}

// Validates: R-6.2.5, R-6.4.2
func TestPlanner_DeleteGuardBelowThresholdProceeds(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 100)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	for i := range 50 {
		require.NoError(t, os.Remove(deleteSafetyFilePath(syncRoot, i)))
	}

	report := runScenarioPass(t, eng)

	assert.Len(t, remote.deletes, 50, "50 deletes is at the threshold, not above it")
	assert.Zero(t, report.HeldRemoteDeletes)
	assert.Empty(t, readHeldDeletes(t, eng))
}

// Validates: R-6.2.5, R-6.4.2
func TestPlanner_DeleteGuardIgnoresMoves(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 100)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// 50 files move with their folder: a reorganization, not data loss.
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "Photos"), filepath.Join(syncRoot, "Pictures")))

	report := runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-photos->Pictures"}, remote.moves)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, report.HeldRemoteDeletes)
}

// Validates: R-6.2.5, R-6.4.2
func TestPlanner_EmptiedLocalSideHoldsEvenFewDeletes(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 3)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// Everything under the sync folder is gone, though the folder itself is
	// the same one (an fscrypt folder with its key unloaded looks like this,
	// and passes the root identity check).
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, entry.Name())))
	}

	report := runScenarioPass(t, eng)

	assert.Empty(t, remote.deletes)
	assert.Equal(t, 6, report.HeldRemoteDeletes, "four files and two folders")
	assert.Empty(t, remote.folderCreates)
}
