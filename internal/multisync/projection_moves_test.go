package multisync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func compiledChildProjectionMoveFixture(t *testing.T, oldPath, newPath string) (*compiledMountSet, config.MountRecord, string) {
	t.Helper()

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.SyncRoot = filepath.Join(t.TempDir(), "parent")
	record := testMountRecordForParent(&parent)
	record.RelativeLocalPath = newPath
	record.ReservedLocalPaths = []string{oldPath}

	inventory := config.DefaultMountInventory()
	inventory.Mounts[record.MountID] = record
	require.NoError(t, config.SaveMountInventory(inventory))

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, inventory)
	require.NoError(t, err)
	require.Len(t, compiled.ProjectionMoves, 1)

	return compiled, record, parent.SyncRoot
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_RenamesLocalProjectionAndClearsReservation(t *testing.T) {
	compiled, record, parentSyncRoot := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldRoot, "kept.txt"), []byte("stateful"), 0o600))

	applyChildProjectionMoves(compiled, nil)

	assert.DirExists(t, newRoot)
	assert.FileExists(t, filepath.Join(newRoot, "kept.txt"))
	assert.NoDirExists(t, oldRoot)
	assert.Empty(t, compiled.Skipped)
	require.Len(t, compiled.Mounts, 2)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Empty(t, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, updated.State)
	assert.Empty(t, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_TargetConflictMarksChildConflictAndSkips(t *testing.T) {
	compiled, record, parentSyncRoot := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.MkdirAll(newRoot, 0o700))

	applyChildProjectionMoves(compiled, nil)

	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalProjectionConflict)
	assert.DirExists(t, oldRoot)
	assert.DirExists(t, newRoot)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, config.MountStateConflict, updated.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionConflict, updated.StateReason)
	assert.Equal(t, []string{"Shortcuts/Old Docs"}, updated.ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_MissingSourceCreatesTargetAndClearsReservation(t *testing.T) {
	compiled, record, parentSyncRoot := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")

	applyChildProjectionMoves(compiled, nil)

	assert.DirExists(t, newRoot)
	assert.Empty(t, compiled.Skipped)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Empty(t, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, updated.State)
}
