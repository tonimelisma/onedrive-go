package multisync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_CreatesMissingRoot(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.DirExists(t, filepath.Join(parent.syncRoot, "Shortcuts", "A"))
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Empty(t, inventory.Mounts[child.MountID].StateReason)
	assert.True(t, inventory.Mounts[child.MountID].LocalRootMaterialized)
	assert.NotNil(t, inventory.Mounts[child.MountID].LocalRootIdentity)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_MissingMaterializedRootUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.NoDirExists(t, filepath.Join(parent.syncRoot, "Shortcuts", "A"))
	assert.Equal(t, config.MountStateUnavailable, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonLocalRootUnavailable, inventory.Mounts[child.MountID].StateReason)
	assert.True(t, inventory.Mounts[child.MountID].LocalRootMaterialized)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_RenamedMaterializedRootCreatesAliasRenameAction(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	childRoot := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	renamedRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	identity, err := rootIdentityForRecordPath(parent, &child)
	require.NoError(t, err)
	child.LocalRootIdentity = identity
	require.NoError(t, os.Rename(childRoot, renamedRoot))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	require.True(t, changed.changed)
	require.Len(t, changed.localRootActions, 1)
	action := changed.localRootActions[0]
	assert.Equal(t, childRootLifecycleActionRename, action.kind)
	assert.Equal(t, "Shortcuts/A", action.fromRelativeLocalPath)
	assert.Equal(t, "Shortcuts/Renamed", action.toRelativeLocalPath)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Empty(t, inventory.Mounts[child.MountID].ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_RetryLocalAliasRenameUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	child.State = config.MountStateUnavailable
	child.StateReason = config.MountStateReasonLocalAliasRenameUnavailable
	child.ReservedLocalPaths = []string{"Shortcuts/Renamed"}
	childRoot := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	renamedRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	identity, err := rootIdentityForRecordPath(parent, &child)
	require.NoError(t, err)
	child.LocalRootIdentity = identity
	require.NoError(t, os.Rename(childRoot, renamedRoot))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	require.True(t, changed.changed)
	require.Len(t, changed.localRootActions, 1)
	action := changed.localRootActions[0]
	assert.Equal(t, childRootLifecycleActionRename, action.kind)
	assert.Equal(t, "Shortcuts/Renamed", action.toRelativeLocalPath)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Equal(t, []string{"Shortcuts/Renamed"}, inventory.Mounts[child.MountID].ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_RetryResolvedLocalAliasRenameConflict(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	child.State = config.MountStateConflict
	child.StateReason = config.MountStateReasonLocalAliasRenameConflict
	child.ReservedLocalPaths = []string{"Shortcuts/Renamed", "Shortcuts/Stale"}
	childRoot := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	renamedRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	identity, err := rootIdentityForRecordPath(parent, &child)
	require.NoError(t, err)
	child.LocalRootIdentity = identity
	require.NoError(t, os.Rename(childRoot, renamedRoot))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	require.True(t, changed.changed)
	require.Len(t, changed.localRootActions, 1)
	action := changed.localRootActions[0]
	assert.Equal(t, childRootLifecycleActionRename, action.kind)
	assert.Equal(t, "Shortcuts/Renamed", action.toRelativeLocalPath)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Equal(t, []string{"Shortcuts/Renamed", "Shortcuts/Stale"}, inventory.Mounts[child.MountID].ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_DeletedMaterializedRootCreatesAliasDeleteAction(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	childRoot := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	identity, err := rootIdentityForRecordPath(parent, &child)
	require.NoError(t, err)
	child.LocalRootIdentity = identity
	require.NoError(t, os.Remove(childRoot))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	require.True(t, changed.changed)
	require.Len(t, changed.localRootActions, 1)
	action := changed.localRootActions[0]
	assert.Equal(t, childRootLifecycleActionDelete, action.kind)
	assert.Equal(t, "Shortcuts/A", action.fromRelativeLocalPath)
	assert.Empty(t, action.toRelativeLocalPath)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_RenamedMaterializedRootUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.LocalRootMaterialized = true
	childRoot := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	renamedRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.Rename(childRoot, renamedRoot))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.NoDirExists(t, childRoot)
	assert.DirExists(t, renamedRoot)
	assert.Equal(t, config.MountStateUnavailable, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonLocalRootUnavailable, inventory.Mounts[child.MountID].StateReason)
	assert.True(t, inventory.Mounts[child.MountID].LocalRootMaterialized)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_FileCollisionConflictsChild(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	root := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(filepath.Dir(root), 0o700))
	require.NoError(t, os.WriteFile(root, []byte("not a directory"), 0o600))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonLocalRootCollision, inventory.Mounts[child.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_SymlinkAncestorConflictsChild(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(parent.syncRoot, "Shortcuts")))
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.NoDirExists(t, filepath.Join(outside, "A"))
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonLocalRootCollision, inventory.Mounts[child.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_PathTraversalConflictsChild(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "../outside")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonLocalRootCollision, inventory.Mounts[child.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileChildMountLocalRoots_ClearsResolvedCollision(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	child.State = config.MountStateConflict
	child.StateReason = config.MountStateReasonLocalRootCollision
	child.LocalRootMaterialized = true
	root := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(root, 0o700))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Empty(t, inventory.Mounts[child.MountID].StateReason)
	assert.True(t, inventory.Mounts[child.MountID].LocalRootMaterialized)
}
