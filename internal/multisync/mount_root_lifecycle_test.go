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

	assert.False(t, changed.changed)
	assert.DirExists(t, filepath.Join(parent.syncRoot, "Shortcuts", "A"))
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Empty(t, inventory.Mounts[child.MountID].StateReason)
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
	root := filepath.Join(parent.syncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(root, 0o700))
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)

	assert.True(t, changed.changed)
	assert.Equal(t, []string{child.MountID}, changed.dirtyMountIDs)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[child.MountID].State)
	assert.Empty(t, inventory.Mounts[child.MountID].StateReason)
}
