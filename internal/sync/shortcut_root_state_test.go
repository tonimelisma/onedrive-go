package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_ApplyShortcutTopologyPersistsParentShortcutRoots(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	changed, err := store.ApplyShortcutTopology(t.Context(), ShortcutTopologyBatch{
		NamespaceID: shortcutTopologyTestNamespaceID,
		Kind:        ShortcutTopologyObservationComplete,
		Upserts: []ShortcutBindingUpsert{{
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
	assert.Equal(t, shortcutTopologyTestNamespaceID, roots[0].NamespaceID)
	assert.Equal(t, "binding-1", roots[0].BindingItemID)
	assert.Equal(t, "Shared/Docs", roots[0].RelativeLocalPath)
	assert.Equal(t, "Docs", roots[0].LocalAlias)
	assert.True(t, roots[0].RemoteDriveID.Equal(driveid.New("drive-1")))
	assert.Equal(t, "target-1", roots[0].RemoteItemID)
	assert.True(t, roots[0].RemoteIsFolder)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), ShortcutTopologyBatch{
		NamespaceID: shortcutTopologyTestNamespaceID,
		Kind:        ShortcutTopologyObservationComplete,
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateRemovedFinalDrain, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_SamePathReplacementWaitsBehindRetiringRoot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), ShortcutTopologyBatch{
		NamespaceID: shortcutTopologyTestNamespaceID,
		Kind:        ShortcutTopologyObservationIncremental,
		Upserts: []ShortcutBindingUpsert{{
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
	assert.Equal(t, ShortcutRootStateSamePathReplacementWaiting, roots[0].State)
	require.NotNil(t, roots[0].Waiting)
	assert.Equal(t, "new-binding", roots[0].Waiting.BindingItemID)
	assert.Equal(t, "Shared/Docs", roots[0].Waiting.RelativeLocalPath)
	assert.True(t, roots[0].Waiting.RemoteDriveID.Equal(driveid.New("new-drive")))
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
	assert.Equal(t, &synctree.FileIdentity{Device: 7, Inode: 9}, roots[0].LocalRootIdentity)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_SamePathUpsertDoesNotDowngradeActiveProtectedOwner(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/New", "Shared/Old"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), ShortcutTopologyBatch{
		NamespaceID: shortcutTopologyTestNamespaceID,
		Kind:        ShortcutTopologyObservationIncremental,
		Upserts: []ShortcutBindingUpsert{{
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
	byBinding := map[string]ShortcutRootRecord{
		roots[0].BindingItemID: roots[0],
		roots[1].BindingItemID: roots[1],
	}
	newRoot := byBinding["new-binding"]
	assert.Equal(t, ShortcutRootStateBlockedPath, newRoot.State)
	assert.Contains(t, newRoot.BlockedDetail, "protected by another shortcut root")
	assert.Nil(t, newRoot.Waiting)
	oldRoot := byBinding["old-binding"]
	assert.Equal(t, ShortcutRootStateActive, oldRoot.State)
	assert.Nil(t, oldRoot.Waiting)
	assert.Equal(t, []string{"Shared/New", "Shared/Old"}, oldRoot.ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_RemoteShortcutRenameKeepsPreviousPathProtected(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Old",
		LocalAlias:        "Old",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Old"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))

	changed, err := store.ApplyShortcutTopology(t.Context(), ShortcutTopologyBatch{
		NamespaceID: shortcutTopologyTestNamespaceID,
		Kind:        ShortcutTopologyObservationIncremental,
		Upserts: []ShortcutBindingUpsert{{
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
func TestSyncStore_AcknowledgeShortcutChildFinalDrainRemovesRetiredRoot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := store.AcknowledgeShortcutChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "binding-1",
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.3, R-2.4.8
func TestSyncStore_AcknowledgeShortcutChildFinalDrainPromotesWaitingReplacement(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateSamePathReplacementWaiting,
		ProtectedPaths:    []string{"Shared/Docs"},
		Waiting: &ShortcutRootReplacement{
			BindingItemID:     "new-binding",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("new-drive"),
			RemoteItemID:      "new-target",
			RemoteIsFolder:    true,
		},
	}}))

	changed, err := store.AcknowledgeShortcutChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "old-binding",
	})
	require.NoError(t, err)
	assert.True(t, changed)

	roots, err := store.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "new-binding", roots[0].BindingItemID)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Nil(t, roots[0].Waiting)
	assert.Equal(t, "Shared/Docs", roots[0].RelativeLocalPath)
	assert.True(t, roots[0].RemoteDriveID.Equal(driveid.New("new-drive")))
}

// Validates: R-2.4.8
func TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState(t *testing.T) {
	t.Parallel()

	var moved struct {
		driveID driveid.ID
		itemID  string
		name    string
	}
	mock := &engineMockClient{
		moveItemFn: func(_ context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.driveID = driveID
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	err := eng.applyShortcutAliasMutation(t.Context(), shortcutAliasMutation{
		Kind:              shortcutAliasMutationRename,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Renamed",
		LocalAlias:        "Renamed",
	})

	require.NoError(t, err)
	assert.True(t, moved.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/Renamed", roots[0].RelativeLocalPath)
	assert.Equal(t, "Renamed", roots[0].LocalAlias)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain(t *testing.T) {
	t.Parallel()

	var deleted struct {
		driveID driveid.ID
		itemID  string
	}
	mock := &engineMockClient{
		deleteItemFn: func(_ context.Context, driveID driveid.ID, itemID string) error {
			deleted.driveID = driveID
			deleted.itemID = itemID
			return nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	err := eng.applyShortcutAliasMutation(t.Context(), shortcutAliasMutation{
		Kind:          shortcutAliasMutationDelete,
		BindingItemID: "binding-1",
	})

	require.NoError(t, err)
	assert.True(t, deleted.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", deleted.itemID)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateRemovedFinalDrain, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestEngine_AcknowledgeChildFinalDrainReleasesParentShortcutRoot(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(aliasRoot, "uploaded.txt"), []byte("drained"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	snapshot, err := eng.AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "binding-1",
	})
	require.NoError(t, err)

	assert.Empty(t, snapshot.Children)
	assert.NoDirExists(t, aliasRoot)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.8
func TestEngine_AcknowledgeChildFinalDrainBlocksWhenAliasProjectionCannotBeRemoved(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(filepath.Dir(aliasRoot), 0o700))
	require.NoError(t, os.WriteFile(aliasRoot, []byte("blocking file"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	snapshot, err := eng.AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "binding-1",
	})

	require.Error(t, err)
	assert.Empty(t, snapshot.Children)
	assert.FileExists(t, aliasRoot)
	roots, listErr := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, listErr)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateRemovedCleanupBlocked, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
	assert.Contains(t, roots[0].BlockedDetail, "not a directory")
}

// Validates: R-2.4.8
func TestEngine_ReconcileMissingLocalAliasDeleteReleasesParentShortcutRoot(t *testing.T) {
	t.Parallel()

	var deleted struct {
		driveID driveid.ID
		itemID  string
	}
	mock := &engineMockClient{
		deleteItemFn: func(_ context.Context, driveID driveid.ID, itemID string) error {
			deleted.driveID = driveID
			deleted.itemID = itemID
			return nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.RemoveAll(aliasRoot))

	changed, err := eng.reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, deleted.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", deleted.itemID)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.8
func TestEngine_EmptyIncrementalTopologyStillReconcilesLocalShortcutAliasRename(t *testing.T) {
	t.Parallel()

	var moved struct {
		itemID string
		name   string
	}
	mock := &engineMockClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	renamedRoot := filepath.Join(syncRoot, "Shared", "Renamed")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.Rename(aliasRoot, renamedRoot))

	var published ShortcutChildTopologyPublication
	eng.shortcutTopologyHandler = func(_ context.Context, publication ShortcutChildTopologyPublication) error {
		published = publication
		return nil
	}
	flow := newEngineFlow(eng.Engine)

	err = flow.applyShortcutTopologyBatch(t.Context(), &remoteObservationBatch{
		shortcutTopology: ShortcutTopologyBatch{
			NamespaceID: shortcutTopologyTestNamespaceID,
			Kind:        ShortcutTopologyObservationIncremental,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	require.Len(t, published.Children, 1)
	assert.Equal(t, "Shared/Renamed", published.Children[0].RelativeLocalPath)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/Renamed", roots[0].RelativeLocalPath)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStateMovesRemoteRenamedProjection(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		moveItemFn: func(context.Context, driveid.ID, string, string, string) (*graph.Item, error) {
			return nil, assert.AnError
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	oldPath := filepath.Join(syncRoot, "Shared", "Old")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldPath, "draft.txt"), []byte("content"), 0o600))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Old"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/New", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))

	changed, err := eng.reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, oldPath)
	assert.FileExists(t, filepath.Join(syncRoot, "Shared", "New", "draft.txt"))
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/New"}, roots[0].ProtectedPaths)
	require.NotNil(t, roots[0].LocalRootIdentity)
	assert.True(t, synctree.SameIdentity(identity, *roots[0].LocalRootIdentity))
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStateMovesRemoteMovedProjectionAcrossLocalParent(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	oldPath := filepath.Join(syncRoot, "Shared", "Old")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldPath, "draft.txt"), []byte("content"), 0o600))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Old"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Archive/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Archive/New", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))

	changed, err := eng.reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, oldPath)
	assert.FileExists(t, filepath.Join(syncRoot, "Archive", "New", "draft.txt"))
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Archive/New"}, roots[0].ProtectedPaths)
	require.NotNil(t, roots[0].LocalRootIdentity)
	assert.True(t, synctree.SameIdentity(identity, *roots[0].LocalRootIdentity))
}
