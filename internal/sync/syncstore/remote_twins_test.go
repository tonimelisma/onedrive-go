package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinsInOneBatchDoNotFailTheCommit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	for _, order := range [][]string{{"composed", "decomposed"}, {"decomposed", "composed"}} {
		mgr := newTestStore(t)
		items := make([]syncdomain.ObservedItem, 0, len(order))
		for _, id := range order {
			items = append(items, syncdomain.ObservedItem{DriveID: driveID, ItemID: id, Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, Hash: "h-" + id, Size: 1})
		}

		// Before: "UNIQUE constraint failed: remote_state.path" failed the
		// whole commit, and every retry of the drive with it.
		require.NoError(t, mgr.CommitObservation(t.Context(), items, "t1", driveID), order)

		holder := readRemoteStateRow(t, mgr.db, order[0])
		twin := readRemoteStateRow(t, mgr.db, order[1])
		require.NotNil(t, holder)
		require.NotNil(t, twin)
		assert.Equal(t, syncfixture.RemoteTwinPath, holder.Path)
		assert.Equal(t, remoteTwinShadowPath(syncfixture.RemoteTwinPath, order[1]), twin.Path)
		assert.Equal(t, []string{syncfixture.RemoteTwinPath}, twinIssuePaths(t, mgr))
	}
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinAcrossBatchesKeepsTheBaselinedHolder(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{DriveID: driveID, ItemID: "synced", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, Hash: "h1", Size: 1},
	}, "t1", driveID))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{DriveID: driveID, ItemID: "newcomer", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, Hash: "h2", Size: 1},
	}, "t2", driveID))

	assert.Equal(t, syncfixture.RemoteTwinPath, readRemoteStateRow(t, mgr.db, "synced").Path, "the first holder keeps the path")
	assert.Equal(t, remoteTwinShadowPath(syncfixture.RemoteTwinPath, "newcomer"), readRemoteStateRow(t, mgr.db, "newcomer").Path)
	assert.Equal(t, []string{syncfixture.RemoteTwinPath}, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinFolderKeepsItsSubtreeApart(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	folder := "ά"
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: syncdomain.ItemTypeFolder},
		{DriveID: driveID, ItemID: "tonos", Path: folder, ItemType: syncdomain.ItemTypeFolder},
		{DriveID: driveID, ItemID: "kid-a", ParentID: "oxia", Path: folder + "/same.txt", ItemType: syncdomain.ItemTypeFile, Hash: "a", Size: 1},
		{DriveID: driveID, ItemID: "kid-b", ParentID: "tonos", Path: folder + "/same.txt", ItemType: syncdomain.ItemTypeFile, Hash: "b", Size: 1},
	}, "t1", driveID))

	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.db, "kid-a").Path)
	assert.Equal(t, remoteTwinShadowPath(folder, "tonos")+"/same.txt", readRemoteStateRow(t, mgr.db, "kid-b").Path,
		"the twin's child lives under the twin, not next to the holder's child")

	// Deleting the holder folder promotes the twin with its subtree intact.
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{DriveID: driveID, ItemID: "oxia", Path: folder, ItemType: syncdomain.ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.db, "kid-a"))
	assert.Equal(t, folder, readRemoteStateRow(t, mgr.db, "tonos").Path)
	assert.Equal(t, folder+"/same.txt", readRemoteStateRow(t, mgr.db, "kid-b").Path)
	assert.Empty(t, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.6
func TestCommitObservation_RemoteTwinRecoversWhenOneIsDeletedOrRenamed(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	twins := func(t *testing.T) *SyncStore {
		t.Helper()
		mgr := newTestStore(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
			{DriveID: driveID, ItemID: "a", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, Hash: "ha", Size: 1},
			{DriveID: driveID, ItemID: "b", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, Hash: "hb", Size: 1},
		}, "t1", driveID))
		return mgr
	}

	t.Run("holder deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
			{DriveID: driveID, ItemID: "a", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, syncfixture.RemoteTwinPath, readRemoteStateRow(t, mgr.db, "b").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	t.Run("twin deleted", func(t *testing.T) {
		t.Parallel()
		mgr := twins(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
			{DriveID: driveID, ItemID: "b", Path: syncfixture.RemoteTwinPath, ItemType: syncdomain.ItemTypeFile, IsDeleted: true},
		}, "t2", driveID))
		assert.Equal(t, syncfixture.RemoteTwinPath, readRemoteStateRow(t, mgr.db, "a").Path)
		assert.Empty(t, twinIssuePaths(t, mgr))
	})

	for _, renamed := range []struct{ name, moved, stays string }{
		{name: "holder renamed away", moved: "a", stays: "b"},
		{name: "twin renamed away", moved: "b", stays: "a"},
	} {
		t.Run(renamed.name, func(t *testing.T) {
			t.Parallel()
			mgr := twins(t)
			require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
				{DriveID: driveID, ItemID: renamed.moved, Path: "renamed.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h" + renamed.moved, Size: 1},
			}, "t2", driveID))
			assert.Equal(t, "renamed.txt", readRemoteStateRow(t, mgr.db, renamed.moved).Path)
			assert.Equal(t, syncfixture.RemoteTwinPath, readRemoteStateRow(t, mgr.db, renamed.stays).Path)
			assert.Empty(t, twinIssuePaths(t, mgr))
		})
	}
}
