package sync

import (
	"context"
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

// Validates: R-2.4.8
func TestEngine_ApplyShortcutAliasMutationRenameMutatesThroughParentAndUpdatesRootState(t *testing.T) {
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

	err := eng.ApplyShortcutAliasMutation(t.Context(), ShortcutAliasMutation{
		Kind:              ShortcutAliasMutationRename,
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
func TestEngine_ApplyShortcutAliasMutationDeleteMarksParentRootFinalDrain(t *testing.T) {
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

	err := eng.ApplyShortcutAliasMutation(t.Context(), ShortcutAliasMutation{
		Kind:          ShortcutAliasMutationDelete,
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
