package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationComplete,
		Upserts: []syncshortcut.ShortcutBindingUpsert{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "drive-1",
			RemoteItemID:      "target-1",
			RemoteIsFolder:    true,
			Complete:          true,
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncfixture.ShortcutNamespaceTestID, roots[0].NamespaceID)
	assert.Equal(t, "binding-1", roots[0].BindingItemID)
	assert.Equal(t, "Shared/Docs", roots[0].RelativeLocalPath)
	assert.Equal(t, "Docs", roots[0].LocalAlias)
	assert.True(t, roots[0].RemoteDriveID.Equal(driveid.New("drive-1")))
	assert.Equal(t, "target-1", roots[0].RemoteItemID)
	assert.True(t, roots[0].RemoteIsFolder)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationComplete,
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedFinalDrain, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_SamePathReplacementWaitsBehindRetiringRoot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9, BirthTime: 11},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
		Upserts: []syncshortcut.ShortcutBindingUpsert{{
			BindingItemID:     "new-binding",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "new-drive",
			RemoteItemID:      "new-target",
			RemoteIsFolder:    true,
			Complete:          true,
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "old-binding", roots[0].BindingItemID)
	assert.Equal(t, syncshortcut.ShortcutRootStateSamePathReplacementWaiting, roots[0].State)
	require.NotNil(t, roots[0].Waiting)
	assert.Equal(t, "new-binding", roots[0].Waiting.BindingItemID)
	assert.Equal(t, "Shared/Docs", roots[0].Waiting.RelativeLocalPath)
	assert.True(t, roots[0].Waiting.RemoteDriveID.Equal(driveid.New("new-drive")))
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
	assert.Equal(t, &synctree.FileIdentity{Device: 7, Inode: 9, BirthTime: 11}, roots[0].LocalRootIdentity,
		"the root's identity keeps its birth time")
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_SamePathUpsertDoesNotDowngradeActiveProtectedOwner(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/New", "Shared/Old"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
		Upserts: []syncshortcut.ShortcutBindingUpsert{{
			BindingItemID:     "new-binding",
			RelativeLocalPath: "Shared/Old",
			LocalAlias:        "Old",
			RemoteDriveID:     "new-drive",
			RemoteItemID:      "new-target",
			RemoteIsFolder:    true,
			Complete:          true,
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 2)
	byBinding := map[string]syncshortcut.ShortcutRootRecord{
		roots[0].BindingItemID: roots[0],
		roots[1].BindingItemID: roots[1],
	}
	newRoot := byBinding["new-binding"]
	assert.Equal(t, syncshortcut.ShortcutRootStateBlockedPath, newRoot.State)
	assert.Contains(t, newRoot.BlockedDetail, "already used by another shared folder")
	assert.Nil(t, newRoot.Waiting)
	oldRoot := byBinding["old-binding"]
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, oldRoot.State)
	assert.Nil(t, oldRoot.Waiting)
	assert.Equal(t, []string{"Shared/New", "Shared/Old"}, oldRoot.ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8, R-2.4.10
func TestSyncStore_DuplicateAutomaticShortcutTargetIsParentBlocked(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationComplete,
		Upserts: []syncshortcut.ShortcutBindingUpsert{
			{
				BindingItemID:     "binding-z",
				RelativeLocalPath: "Shared/Zed",
				LocalAlias:        "Zed",
				RemoteDriveID:     "drive-1",
				RemoteItemID:      "target-1",
				RemoteIsFolder:    true,
				Complete:          true,
			},
			{
				BindingItemID:     "binding-a",
				RelativeLocalPath: "Shared/Alpha",
				LocalAlias:        "Alpha",
				RemoteDriveID:     "drive-1",
				RemoteItemID:      "target-1",
				RemoteIsFolder:    true,
				Complete:          true,
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 2)
	byBinding := map[string]syncshortcut.ShortcutRootRecord{
		roots[0].BindingItemID: roots[0],
		roots[1].BindingItemID: roots[1],
	}
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, byBinding["binding-a"].State)
	duplicate := byBinding["binding-z"]
	assert.Equal(t, syncshortcut.ShortcutRootStateDuplicateTarget, duplicate.State)
	assert.Contains(t, duplicate.BlockedDetail, "Shared/Alpha", "names where the winning shortcut syncs")
	assert.NotContains(t, duplicate.BlockedDetail, "binding-a", "raw OneDrive item IDs stay out of status text")
	assert.Equal(t, []string{"Shared/Zed"}, duplicate.ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_RemoteShortcutRenameKeepsPreviousPathProtected(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Old",
		LocalAlias:        "Old",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Old"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
		Upserts: []syncshortcut.ShortcutBindingUpsert{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/New",
			LocalAlias:        "New",
			RemoteDriveID:     "drive-1",
			RemoteItemID:      "target-1",
			RemoteIsFolder:    true,
			Complete:          true,
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/New", roots[0].RelativeLocalPath)
	assert.Equal(t, []string{"Shared/New", "Shared/Old"}, roots[0].ProtectedPaths)
	assert.Equal(t, &synctree.FileIdentity{Device: 7, Inode: 9}, roots[0].LocalRootIdentity)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := store.MarkShortcutChildFinalDrainReleasePending(t.Context(), syncshortcut.ShortcutChildDrainAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedReleasePending, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_acknowledgeShortcutChildArtifactsPurgedRemovesCleanupPendingRoot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedChildCleanupPending,
	}}))

	changed, err := store.AcknowledgeShortcutChildArtifactsPurged(t.Context(), syncshortcut.ShortcutChildArtifactCleanupAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_RemoteUpsertRestoresCleanupPendingRoot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedChildCleanupPending,
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), syncshortcut.ShortcutTopologyBatch{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
		Upserts: []syncshortcut.ShortcutBindingUpsert{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "new-drive",
			RemoteItemID:      "new-target",
			RemoteIsFolder:    true,
			Complete:          true,
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
	assert.True(t, roots[0].RemoteDriveID.Equal(driveid.New("new-drive")))
	snapshot, err := store.ShortcutChildWorkSnapshot(t.Context(), syncfixture.ShortcutNamespaceTestID, t.TempDir())
	require.NoError(t, err)
	require.Len(t, snapshot.RunCommands, 1)
	assert.Empty(t, snapshot.CleanupCommands)
}
