package sync

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func seedShortcutLocalStateIdentityForTest(t *testing.T, eng *Engine, relativePath string) {
	t.Helper()

	identity, err := eng.syncTree.IdentityNoFollow(filepath.FromSlash(relativePath))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceLocalState(t.Context(), []syncdomain.LocalStateRow{{
		Path:             relativePath,
		ItemType:         syncdomain.ItemTypeFolder,
		LocalDevice:      identity.Device,
		LocalInode:       identity.Inode,
		LocalBirthTime:   identity.BirthTime,
		LocalHasIdentity: true,
	}}))
}

func shortcutRootByBindingForTest(records []syncshortcut.ShortcutRootRecord) map[string]syncshortcut.ShortcutRootRecord {
	byBinding := make(map[string]syncshortcut.ShortcutRootRecord, len(records))
	for i := range records {
		byBinding[records[i].BindingItemID] = records[i]
	}
	return byBinding
}

// Validates: R-2.4.8
func TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState(t *testing.T) {
	t.Parallel()

	var moved struct {
		driveID driveid.ID
		itemID  string
		name    string
	}
	mock := &syncfixture.EngineMockClient{
		MoveItemFn: func(_ context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.driveID = driveID
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
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

	err := eng.shortcutRoots().applyShortcutAliasMutation(t.Context(), syncshortcut.ShortcutAliasMutation{
		Kind:              syncshortcut.ShortcutAliasMutationRename,
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
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain(t *testing.T) {
	t.Parallel()

	var deleted struct {
		driveID driveid.ID
		itemID  string
	}
	mock := &syncfixture.EngineMockClient{
		DeleteItemFn: func(_ context.Context, driveID driveid.ID, itemID string) error {
			deleted.driveID = driveID
			deleted.itemID = itemID
			return nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
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

	err := eng.shortcutRoots().applyShortcutAliasMutation(t.Context(), syncshortcut.ShortcutAliasMutation{
		Kind:          syncshortcut.ShortcutAliasMutationDelete,
		BindingItemID: "binding-1",
	})

	require.NoError(t, err)
	assert.True(t, deleted.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", deleted.itemID)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedFinalDrain, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.3, R-2.4.8
func TestEngine_AcknowledgeChildFinalDrainReleasesParentShortcutRoot(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	// The child's final drain released the tree, so the root is empty.
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
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

	snapshot, err := eng.ShortcutChildAckHandle().AcknowledgeChildFinalDrain(t.Context(), syncshortcut.ShortcutChildDrainAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})
	require.NoError(t, err)

	assert.Empty(t, snapshot.RunCommands)
	require.Len(t, snapshot.CleanupCommands, 1)
	assert.Equal(t, "personal:owner@example.com|binding:binding-1", snapshot.CleanupCommands[0].ChildMountID)
	assert.NoDirExists(t, aliasRoot)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedChildCleanupPending, roots[0].State)
	assert.Empty(t, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
//
// The parent never walks a removed shared folder's tree. A root that still
// holds something when the child acknowledges its drain (it arrived after the
// child's release) stays as it is, and the binding goes back to the final
// drain, so the child runs again and releases or reports what is there.
func TestEngine_AcknowledgeChildFinalDrainSendsANonEmptyRootBackToTheFinalDrain(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(filepath.Join(aliasRoot, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(aliasRoot, "sub", "arrived-late.txt"), []byte("mine"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
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

	snapshot, err := eng.ShortcutChildAckHandle().AcknowledgeChildFinalDrain(t.Context(), syncshortcut.ShortcutChildDrainAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})

	require.NoError(t, err, "a root that is not empty is not a failed release")
	assert.FileExists(t, filepath.Join(aliasRoot, "sub", "arrived-late.txt"), "nothing under the root is touched")
	assert.Empty(t, snapshot.CleanupCommands, "the child's state is not purged")
	require.Len(t, snapshot.RunCommands, 1, "the final drain runs again")
	assert.Equal(t, syncshortcut.ShortcutChildRunModeFinalDrain, snapshot.RunCommands[0].Mode)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedFinalDrain, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)

	// Nothing changed: the next local reconcile leaves the record alone.
	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
	require.NoError(t, err)
	assert.False(t, changed)
	assert.FileExists(t, filepath.Join(aliasRoot, "sub", "arrived-late.txt"))
}

// Validates: R-2.4.8
func TestEngine_ReconcileRemovedFinalDrainMissingLocalAliasReleasesWithoutRemoteDelete(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{
		DeleteItemFn: func(context.Context, driveid.ID, string) error {
			require.FailNow(t, "manual discard must not delete the remote shortcut or target")
			return nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.RemoveAll(aliasRoot))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.8
func TestEngine_AcknowledgeChildFinalDrainBlocksWhenAliasProjectionCannotBeRemoved(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(filepath.Dir(aliasRoot), 0o700))
	require.NoError(t, os.WriteFile(aliasRoot, []byte("blocking file"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
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

	snapshot, err := eng.ShortcutChildAckHandle().AcknowledgeChildFinalDrain(t.Context(), syncshortcut.ShortcutChildDrainAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})

	require.Error(t, err)
	assert.Empty(t, snapshot.RunCommands)
	assert.FileExists(t, aliasRoot)
	roots, listErr := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, listErr)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedCleanupBlocked, roots[0].State)
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
	mock := &syncfixture.EngineMockClient{
		DeleteItemFn: func(_ context.Context, driveID driveid.ID, itemID string) error {
			deleted.driveID = driveID
			deleted.itemID = itemID
			return nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.RemoveAll(aliasRoot))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, deleted.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", deleted.itemID)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.8
func TestEngine_ReconcileMissingAliasIgnoresMissingHistoricalProtectedPathBeforeDelete(t *testing.T) {
	t.Parallel()

	var deleted struct {
		driveID driveid.ID
		itemID  string
	}
	mock := &syncfixture.EngineMockClient{
		DeleteItemFn: func(_ context.Context, driveID driveid.ID, itemID string) error {
			deleted.driveID = driveID
			deleted.itemID = itemID
			return nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.RemoveAll(aliasRoot))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, deleted.driveID.Equal(testThrottleDriveID()))
	assert.Equal(t, "binding-1", deleted.itemID)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// Validates: R-2.4.8
func TestEngine_ReconcileMissingAliasIgnoresMissingHistoricalProtectedPathBeforeRename(t *testing.T) {
	t.Parallel()

	var moved struct {
		itemID string
		name   string
	}
	mock := &syncfixture.EngineMockClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	renamedRoot := filepath.Join(syncRoot, "Shared", "Renamed")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.Rename(aliasRoot, renamedRoot))
	seedShortcutLocalStateIdentityForTest(t, eng.Engine, "Shared/Renamed")

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/Renamed", roots[0].RelativeLocalPath)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs", "Shared/Old"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ReconcileMissingAliasDetectsLiveRenameWithoutStaleLocalState(t *testing.T) {
	t.Parallel()

	var moved struct {
		itemID string
		name   string
	}
	mock := &syncfixture.EngineMockClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	renamedRoot := filepath.Join(syncRoot, "Shared", "Renamed")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.Rename(aliasRoot, renamedRoot))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/Renamed", roots[0].RelativeLocalPath)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_EmptyIncrementalTopologyStillReconcilesLocalShortcutAliasRename(t *testing.T) {
	t.Parallel()

	var moved struct {
		itemID string
		name   string
	}
	mock := &syncfixture.EngineMockClient{
		MoveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	renamedRoot := filepath.Join(syncRoot, "Shared", "Renamed")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.Rename(aliasRoot, renamedRoot))
	seedShortcutLocalStateIdentityForTest(t, eng.Engine, "Shared/Renamed")

	var published syncshortcut.ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication syncshortcut.ShortcutChildWorkSnapshot) error {
		published = publication
		return nil
	}
	flow := newEngineFlow(eng.flowInputs())

	err = flow.applyShortcutObservationBatch(t.Context(), &syncobserve.RemoteObservationBatch{
		ShortcutTopology: syncshortcut.ShortcutTopologyBatch{
			NamespaceID: syncfixture.ShortcutNamespaceTestID,
			Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	require.Len(t, published.RunCommands, 1)
	assert.Equal(t, filepath.Join(syncRoot, "Shared", "Renamed"), published.RunCommands[0].Engine.LocalRoot)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "Shared/Renamed", roots[0].RelativeLocalPath)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStateMovesRemoteRenamedProjection(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{
		MoveItemFn: func(context.Context, driveid.ID, string, string, string) (*graph.Item, error) {
			return nil, assert.AnError
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	oldPath := filepath.Join(syncRoot, "Shared", "Old")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldPath, "draft.txt"), []byte("content"), 0o600))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Old"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/New", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))
	seedShortcutLocalStateIdentityForTest(t, eng.Engine, "Shared/Old")

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, oldPath)
	assert.FileExists(t, filepath.Join(syncRoot, "Shared", "New", "draft.txt"))
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Shared/New"}, roots[0].ProtectedPaths)
	require.NotNil(t, roots[0].LocalRootIdentity)
	assert.True(t, synctree.SameIdentity(identity, *roots[0].LocalRootIdentity))
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStateMovesRemoteMovedProjectionAcrossLocalParent(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	oldPath := filepath.Join(syncRoot, "Shared", "Old")
	require.NoError(t, os.MkdirAll(oldPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldPath, "draft.txt"), []byte("content"), 0o600))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Old"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Archive/New",
		LocalAlias:        "New",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{"Archive/New", "Shared/Old"},
		LocalRootIdentity: &identity,
	}}))
	seedShortcutLocalStateIdentityForTest(t, eng.Engine, "Shared/Old")

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, oldPath)
	assert.FileExists(t, filepath.Join(syncRoot, "Archive", "New", "draft.txt"))
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
	assert.Equal(t, []string{"Archive/New"}, roots[0].ProtectedPaths)
	require.NotNil(t, roots[0].LocalRootIdentity)
	assert.True(t, synctree.SameIdentity(identity, *roots[0].LocalRootIdentity))
}

// A shared folder's shortcut folder sits in the parent's sync tree, and so
// does a parent a moved one needs: folders in the user's tree like any other
// sync creates, asked for 0777 and given the umask. They used to be 0700
// whatever the umask. A folder that already exists keeps its mode.
//
// Validates: R-5.10.2, R-2.4.8
func TestEngine_ShortcutRootFoldersFollowTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	shortcutRoot := func(relativePath string, protected []string, identity *synctree.FileIdentity) syncshortcut.ShortcutRootRecord {
		return syncshortcut.ShortcutRootRecord{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-1",
			RelativeLocalPath: relativePath,
			LocalAlias:        filepath.Base(relativePath),
			RemoteDriveID:     driveid.New("drive-1"),
			RemoteItemID:      "target-1",
			RemoteIsFolder:    true,
			State:             syncshortcut.ShortcutRootStateActive,
			ProtectedPaths:    protected,
			LocalRootIdentity: identity,
		}
	}
	folderMode := func(t *testing.T, syncRoot, rel string) os.FileMode {
		t.Helper()
		info, err := os.Stat(filepath.Join(syncRoot, filepath.FromSlash(rel)))
		require.NoError(t, err)
		return info.Mode().Perm()
	}

	t.Run("a shortcut folder and its parent", func(t *testing.T) {
		eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
		eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
		eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
		require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{
			shortcutRoot("Shared/Docs", []string{"Shared/Docs"}, nil),
		}))

		_, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
		require.NoError(t, err)

		assert.Equal(t, os.FileMode(0o775), folderMode(t, syncRoot, "Shared"), "0777 minus the umask")
		assert.Equal(t, os.FileMode(0o775), folderMode(t, syncRoot, "Shared/Docs"), "0777 minus the umask")
	})

	t.Run("the parent a moved shortcut folder needs", func(t *testing.T) {
		eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
		eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
		eng.protectedRoots.namespaceID = syncfixture.ShortcutNamespaceTestID
		require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "Shared", "Old"), 0o700))
		identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Old"))
		require.NoError(t, err)
		require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{
			shortcutRoot("Archive/New", []string{"Archive/New", "Shared/Old"}, &identity),
		}))
		seedShortcutLocalStateIdentityForTest(t, eng.Engine, "Shared/Old")

		_, err = eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
		require.NoError(t, err)

		assert.Equal(t, os.FileMode(0o775), folderMode(t, syncRoot, "Archive"), "0777 minus the umask")
		assert.Equal(t, os.FileMode(0o700), folderMode(t, syncRoot, "Archive/New"), "the moved folder keeps its mode")
	})
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStateRetriesRemovedReleasePending(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, aliasRoot)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedChildCleanupPending, roots[0].State)
	assert.Empty(t, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
// Regression: entering the cleanup-blocked state used to return the cleanup
// error once, which in watch mode ended the remote observer. The transition
// now persists the blocked state, warns once, and returns no error.
func TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedWithoutError(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	var logBuf bytes.Buffer
	eng.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(filepath.Dir(aliasRoot), 0o700))
	require.NoError(t, os.WriteFile(aliasRoot, []byte("blocking file"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err, "a blocked cleanup is durable state, not a pass failure")
	assert.True(t, changed)
	roots, listErr := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, listErr)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedCleanupBlocked, roots[0].State)
	assert.Contains(t, roots[0].BlockedDetail, "not a directory")
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
	assert.Equal(t, 1, strings.Count(logBuf.String(), "level=WARN"), "the transition warns exactly once")
	assert.Contains(t, logBuf.String(), "Could not remove the local copy of removed shared folder Shared/Docs")
}

// Validates: R-2.4.8
// Regression: an unchanged blocked cleanup used to return its error on every
// pass, failing each one-shot run and ending the watch remote observer.
func TestEngine_ReconcileShortcutRootLocalStateRepeatedCleanupBlockedIsQuiet(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	var logBuf bytes.Buffer
	eng.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(filepath.Dir(aliasRoot), 0o700))
	require.NoError(t, os.WriteFile(aliasRoot, []byte("still blocking"), 0o600))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
	require.NoError(t, err)
	assert.True(t, changed, "the transition into the blocked state is persisted")

	for range 3 {
		changed, repeatErr := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
		require.NoError(t, repeatErr, "the unchanged blocked state is already persisted and shown by status")
		assert.False(t, changed)
	}
	assert.Equal(t, 1, strings.Count(logBuf.String(), "level=WARN"), "only the transition warns")
	roots, listErr := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, listErr)
	require.Len(t, roots, 1)
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedCleanupBlocked, roots[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, roots[0].ProtectedPaths)
}

// Validates: R-2.4.8
func TestEngine_ReconcileShortcutRootLocalStatePromotesWaitingReplacementAfterReleasePending(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("old-drive"),
		RemoteItemID:      "old-target",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
		Waiting: &syncshortcut.ShortcutRootReplacement{
			BindingItemID:     "new-binding",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("new-drive"),
			RemoteItemID:      "new-target",
			RemoteIsFolder:    true,
		},
	}}))

	changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())

	require.NoError(t, err)
	assert.True(t, changed)
	assert.NoDirExists(t, aliasRoot)
	roots, err := eng.baseline.ListShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 2)
	byBinding := shortcutRootByBindingForTest(roots)
	require.Contains(t, byBinding, "old-binding")
	assert.Equal(t, syncshortcut.ShortcutRootStateRemovedChildCleanupPending, byBinding["old-binding"].State)
	assert.Empty(t, byBinding["old-binding"].ProtectedPaths)
	require.Contains(t, byBinding, "new-binding")
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, byBinding["new-binding"].State)
	assert.True(t, byBinding["new-binding"].RemoteDriveID.Equal(driveid.New("new-drive")))
}

// excludedShortcutAlias is the alias path of the shortcut the filter tests
// exclude or keep.
const excludedShortcutAlias = "Shared/Docs"

// excludedShortcutRoot is the active shortcut root at excludedShortcutAlias.
func excludedShortcutRoot(identity *synctree.FileIdentity) syncshortcut.ShortcutRootRecord {
	return syncshortcut.ShortcutRootRecord{
		NamespaceID:       syncfixture.ShortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: excludedShortcutAlias,
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             syncshortcut.ShortcutRootStateActive,
		ProtectedPaths:    []string{excludedShortcutAlias},
		LocalRootIdentity: identity,
	}
}

// shortcutFilterCase is a content filter and whether it excludes the shortcut.
type shortcutFilterCase struct {
	name     string
	filter   syncdomain.ContentFilterConfig
	excluded bool
}

func shortcutFilterCases() []shortcutFilterCase {
	return []shortcutFilterCase{
		{name: "ignored_dirs names the shortcut's parent", filter: syncdomain.ContentFilterConfig{IgnoredDirs: []string{"Shared"}}, excluded: true},
		{name: "ignored_paths names the shortcut", filter: syncdomain.ContentFilterConfig{IgnoredPaths: []string{excludedShortcutAlias}}, excluded: true},
		{name: "included_dirs leaves the shortcut out", filter: syncdomain.ContentFilterConfig{IncludedDirs: []string{"Work"}}, excluded: true},
		{name: "a filter that hides nothing here", filter: syncdomain.ContentFilterConfig{IgnoredDirs: []string{"Other"}}},
	}
}

// A shortcut whose path the user's filters exclude is not touched: a pass of
// the parent's local lifecycle makes no folder for it and leaves a folder
// already there as it is, the child work the parent publishes has no run
// command for it, and a materialized root whose folder is gone is not read as
// the user deleting the alias. A filter that hides nothing gets the folder
// and the run command.
//
// Validates: R-2.4.18, R-2.4.8
func TestEngine_ShortcutTheFiltersExcludeLeavesNoFolderAndNoWork(t *testing.T) {
	t.Parallel()

	for _, tc := range shortcutFilterCases() {
		t.Run(tc.name+": a new shortcut", func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
			eng.contentFilter = tc.filter
			var published []syncshortcut.ShortcutChildWorkSnapshot
			eng.shortcutChildWorkSink = func(_ context.Context, snapshot syncshortcut.ShortcutChildWorkSnapshot) error {
				published = append(published, snapshot)
				return nil
			}
			require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{excludedShortcutRoot(nil)}))

			_, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
			require.NoError(t, err)
			returned, err := eng.shortcutRoots().acknowledgeChildArtifactsPurged(t.Context(), syncshortcut.ShortcutChildArtifactCleanupAck{})
			require.NoError(t, err)

			require.Len(t, published, 1)
			roots, err := eng.baseline.ListShortcutRoots(t.Context())
			require.NoError(t, err)
			require.Len(t, roots, 1)
			if tc.excluded {
				assert.NoDirExists(t, filepath.Join(syncRoot, "Shared"), "no folder is made under an excluded path")
				assert.Empty(t, published[0].RunCommands, "an excluded shortcut has no child run")
				assert.Empty(t, returned.RunCommands)
				assert.Nil(t, roots[0].LocalRootIdentity, "the record is left as it was")
				return
			}
			assert.DirExists(t, filepath.Join(syncRoot, "Shared", "Docs"))
			require.Len(t, published[0].RunCommands, 1)
			assert.NotNil(t, roots[0].LocalRootIdentity)
		})
	}

}

// Validates: R-2.4.18
func TestEngine_ShortcutTheFiltersExcludeLeavesAFolderAlreadyThereAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range shortcutFilterCases() {
		if !tc.excluded {
			continue
		}
		t.Run(tc.name+": a folder already there", func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), withTestShortcutNamespace)
			eng.contentFilter = tc.filter
			kept := filepath.Join(syncRoot, "Shared", "Docs", "notes.txt")
			require.NoError(t, os.MkdirAll(filepath.Dir(kept), 0o700))
			require.NoError(t, os.WriteFile(kept, []byte("the user's own"), 0o600))
			require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{excludedShortcutRoot(nil)}))

			changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
			require.NoError(t, err)

			assert.False(t, changed)
			content, err := os.ReadFile(kept) //nolint:gosec // the test reads the folder it made
			require.NoError(t, err)
			assert.Equal(t, "the user's own", string(content))
		})
	}
}

// Validates: R-2.4.18, R-2.4.8
func TestEngine_ShortcutTheFiltersExcludeIsNotReadAsTheAliasDeleted(t *testing.T) {
	t.Parallel()

	for _, tc := range shortcutFilterCases() {
		if !tc.excluded {
			continue
		}
		t.Run(tc.name+": a materialized folder that is gone", func(t *testing.T) {
			t.Parallel()

			var deletedItems []string
			mock := &syncfixture.EngineMockClient{
				DeleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
					deletedItems = append(deletedItems, itemID)
					return nil
				},
			}
			eng, syncRoot := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), withTestShortcutNamespace)
			eng.contentFilter = tc.filter
			gone := filepath.Join(syncRoot, "Shared", "Docs")
			require.NoError(t, os.MkdirAll(gone, 0o700))
			identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
			require.NoError(t, err)
			require.NoError(t, os.Remove(gone))
			require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{excludedShortcutRoot(&identity)}))

			changed, err := eng.shortcutRoots().reconcileShortcutRootLocalState(t.Context())
			require.NoError(t, err)

			assert.False(t, changed)
			assert.Empty(t, deletedItems, "the alias is not deleted on OneDrive")
			roots, err := eng.baseline.ListShortcutRoots(t.Context())
			require.NoError(t, err)
			require.Len(t, roots, 1)
			assert.Equal(t, syncshortcut.ShortcutRootStateActive, roots[0].State)
		})
	}
}
