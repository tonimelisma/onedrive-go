package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// The live pair from probe L-33: OneDrive keeps U+FB2A and U+05E9 U+05C1 as
// two items; both are stored at the NFC path U+05E9 U+05C1.
const remoteTwinPath = "שׁ.txt"

func twinIssuePaths(t *testing.T, mgr *SyncStore) []string {
	t.Helper()

	issues, err := mgr.ListObservationIssues(t.Context())
	require.NoError(t, err)
	var paths []string
	for i := range issues {
		if issues[i].IssueType == issueRemoteNameTwin {
			paths = append(paths, issues[i].Path)
		}
	}

	return paths
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinsInOneBatchDoNotFailTheCommit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	for _, order := range [][]string{{"composed", "decomposed"}, {"decomposed", "composed"}} {
		mgr := newTestStore(t)
		items := make([]observedItem, 0, len(order))
		for _, id := range order {
			items = append(items, observedItem{DriveID: driveID, ItemID: id, Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h-" + id, Size: 1})
		}

		// Before: "UNIQUE constraint failed: remote_state.path" failed the
		// whole commit, and every retry of the drive with it.
		require.NoError(t, mgr.CommitObservation(t.Context(), items, "t1", driveID), order)

		holder := readRemoteStateRow(t, mgr.rawDB(), order[0])
		twin := readRemoteStateRow(t, mgr.rawDB(), order[1])
		require.NotNil(t, holder)
		require.NotNil(t, twin)
		assert.Equal(t, remoteTwinPath, holder.Path)
		assert.Equal(t, remoteTwinShadowPath(remoteTwinPath, order[1]), twin.Path)
		assert.Equal(t, []string{remoteTwinPath}, twinIssuePaths(t, mgr))
	}
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinAcrossBatchesKeepsTheBaselinedHolder(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{DriveID: driveID, ItemID: "synced", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h1", Size: 1},
	}, "t1", driveID))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{DriveID: driveID, ItemID: "newcomer", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h2", Size: 1},
	}, "t2", driveID))

	assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.rawDB(), "synced").Path, "the first holder keeps the path")
	assert.Equal(t, remoteTwinShadowPath(remoteTwinPath, "newcomer"), readRemoteStateRow(t, mgr.rawDB(), "newcomer").Path)
	assert.Equal(t, []string{remoteTwinPath}, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinFolderKeepsItsSubtreeApart(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	folder := "ά"
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "tonos", Path: folder, ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "kid-a", ParentID: "oxia", Path: folder + "/same.txt", ItemType: ItemTypeFile, Hash: "a", Size: 1},
		{DriveID: driveID, ItemID: "kid-b", ParentID: "tonos", Path: folder + "/same.txt", ItemType: ItemTypeFile, Hash: "b", Size: 1},
	}, "t1", driveID))

	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.rawDB(), "kid-a").Path)
	assert.Equal(t, remoteTwinShadowPath(folder, "tonos")+"/same.txt", readRemoteStateRow(t, mgr.rawDB(), "kid-b").Path,
		"the twin's child lives under the twin, not next to the holder's child")

	// Deleting the holder folder promotes the twin with its subtree intact.
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "kid-a"))
	assert.Equal(t, folder, readRemoteStateRow(t, mgr.rawDB(), "tonos").Path)
	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.rawDB(), "kid-b").Path)
	assert.Empty(t, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinRecoversWhenOneIsDeletedOrRenamed(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	twins := func(t *testing.T) *SyncStore {
		t.Helper()
		mgr := newTestStore(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
			{DriveID: driveID, ItemID: "a", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "ha", Size: 1},
			{DriveID: driveID, ItemID: "b", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "hb", Size: 1},
		}, "t1", driveID))
		return mgr
	}

	t.Run("holder deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
			{DriveID: driveID, ItemID: "a", Path: remoteTwinPath, ItemType: ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.rawDB(), "b").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	t.Run("twin deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
			{DriveID: driveID, ItemID: "b", Path: remoteTwinPath, ItemType: ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.rawDB(), "a").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	for _, renamed := range []struct{ name, moved, stays string }{
		{name: "holder renamed away", moved: "a", stays: "b"},
		{name: "twin renamed away", moved: "b", stays: "a"},
	} {
		t.Run(renamed.name, func(t *testing.T) {
			t.Parallel()
			mgr := twins(t)
			require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
				{DriveID: driveID, ItemID: renamed.moved, Path: "renamed.txt", ItemType: ItemTypeFile, Hash: "h" + renamed.moved, Size: 1},
			}, "t2", driveID))
			assert.Equal(t, "renamed.txt", readRemoteStateRow(t, mgr.rawDB(), renamed.moved).Path)
			assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.rawDB(), renamed.stays).Path)
			assert.Empty(t, twinIssuePaths(t, mgr))
		})
	}
}

// Validates: R-2.12.6
func TestRunOnce_RemoteTwinsAreHeldAndTheDriveKeepsSyncing(t *testing.T) {
	t.Parallel()

	// Live: the installed build looped "UNIQUE constraint failed:
	// remote_state.path" for the whole drive after probe L-33 left these two
	// items in one folder.
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "twin-composed", "root", "שׁ.txt", "composed\n")
	remote.addFile(t, "twin-decomposed", "root", "שׁ.txt", "decomposed\n")
	remote.addFile(t, "other", "root", "other.txt", "other\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	assert.FileExists(t, filepath.Join(syncRoot, "other.txt"), "one remote fact must not stop the drive")
	assert.NoFileExists(t, filepath.Join(syncRoot, remoteTwinPath), "neither twin syncs")
	requireScenarioIssue(t, eng, remoteTwinPath, issueRemoteNameTwin)

	// Deleting one twin on OneDrive lets the survivor sync.
	remote.mu.Lock()
	remote.remove("twin-composed")
	remote.mu.Unlock()
	runScenarioPass(t, eng)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, remoteTwinPath))
	require.NoError(t, err)
	assert.Equal(t, "decomposed\n", string(data))
	assert.Empty(t, twinIssuePaths(t, eng.baseline))
}

// Validates: R-2.12.6
func TestRunOnce_RemoteTwinOfASyncedFileKeepsTheLocalFile(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "synced", "root", "שׁ.txt", "synced\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, remoteTwinPath))

	remote.mu.Lock()
	remote.addFile(t, "newcomer", "root", "שׁ.txt", "newcomer\n")
	remote.mu.Unlock()
	runScenarioPass(t, eng)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, remoteTwinPath))
	require.NoError(t, err, "a twin being held must never delete the synced file")
	assert.Equal(t, "synced\n", string(data))
	assert.Empty(t, remote.deletes)
	requireScenarioIssue(t, eng, remoteTwinPath, issueRemoteNameTwin)
}
