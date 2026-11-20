package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinsInOneBatchDoNotFailTheCommit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	for _, order := range [][]string{{"composed", "decomposed"}, {"decomposed", "composed"}} {
		mgr := newTestStore(t)
		items := make([]ObservedItem, 0, len(order))
		for _, id := range order {
			items = append(items, ObservedItem{DriveID: driveID, ItemID: id, Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h-" + id, Size: 1})
		}

		// Before: "UNIQUE constraint failed: remote_state.path" failed the
		// whole commit, and every retry of the drive with it.
		require.NoError(t, mgr.CommitObservation(t.Context(), items, "t1", driveID), order)

		holder := readRemoteStateRow(t, mgr.RawDBForTest(), order[0])
		twin := readRemoteStateRow(t, mgr.RawDBForTest(), order[1])
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
	require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
		{DriveID: driveID, ItemID: "synced", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h1", Size: 1},
	}, "t1", driveID))

	require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
		{DriveID: driveID, ItemID: "newcomer", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "h2", Size: 1},
	}, "t2", driveID))

	assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.RawDBForTest(), "synced").Path, "the first holder keeps the path")
	assert.Equal(t, remoteTwinShadowPath(remoteTwinPath, "newcomer"), readRemoteStateRow(t, mgr.RawDBForTest(), "newcomer").Path)
	assert.Equal(t, []string{remoteTwinPath}, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinFolderKeepsItsSubtreeApart(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	folder := "ά"
	require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "tonos", Path: folder, ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "kid-a", ParentID: "oxia", Path: folder + "/same.txt", ItemType: ItemTypeFile, Hash: "a", Size: 1},
		{DriveID: driveID, ItemID: "kid-b", ParentID: "tonos", Path: folder + "/same.txt", ItemType: ItemTypeFile, Hash: "b", Size: 1},
	}, "t1", driveID))

	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.RawDBForTest(), "kid-a").Path)
	assert.Equal(t, remoteTwinShadowPath(folder, "tonos")+"/same.txt", readRemoteStateRow(t, mgr.RawDBForTest(), "kid-b").Path,
		"the twin's child lives under the twin, not next to the holder's child")

	// Deleting the holder folder promotes the twin with its subtree intact.
	require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.RawDBForTest(), "kid-a"))
	assert.Equal(t, folder, readRemoteStateRow(t, mgr.RawDBForTest(), "tonos").Path)
	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.RawDBForTest(), "kid-b").Path)
	assert.Empty(t, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinRecoversWhenOneIsDeletedOrRenamed(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	twins := func(t *testing.T) *SyncStore {
		t.Helper()
		mgr := newTestStore(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
			{DriveID: driveID, ItemID: "a", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "ha", Size: 1},
			{DriveID: driveID, ItemID: "b", Path: remoteTwinPath, ItemType: ItemTypeFile, Hash: "hb", Size: 1},
		}, "t1", driveID))
		return mgr
	}

	t.Run("holder deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
			{DriveID: driveID, ItemID: "a", Path: remoteTwinPath, ItemType: ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.RawDBForTest(), "b").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	t.Run("twin deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
			{DriveID: driveID, ItemID: "b", Path: remoteTwinPath, ItemType: ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.RawDBForTest(), "a").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	for _, renamed := range []struct{ name, moved, stays string }{
		{name: "holder renamed away", moved: "a", stays: "b"},
		{name: "twin renamed away", moved: "b", stays: "a"},
	} {
		t.Run(renamed.name, func(t *testing.T) {
			t.Parallel()
			mgr := twins(t)
			require.NoError(t, mgr.CommitObservation(t.Context(), []ObservedItem{
				{DriveID: driveID, ItemID: renamed.moved, Path: "renamed.txt", ItemType: ItemTypeFile, Hash: "h" + renamed.moved, Size: 1},
			}, "t2", driveID))
			assert.Equal(t, "renamed.txt", readRemoteStateRow(t, mgr.RawDBForTest(), renamed.moved).Path)
			assert.Equal(t, remoteTwinPath, readRemoteStateRow(t, mgr.RawDBForTest(), renamed.stays).Path)
			assert.Empty(t, twinIssuePaths(t, mgr))
		})
	}
}
