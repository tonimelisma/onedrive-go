package multisync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func compiledChildProjectionMoveFixture(t *testing.T, oldPath, newPath string) (
	*compiledMountSet,
	config.MountRecord,
	string,
	StandaloneMountConfig,
) {
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

	return compiled, record, parent.SyncRoot, parent
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_RenamesLocalProjectionAndClearsReservation(t *testing.T) {
	compiled, record, parentSyncRoot, parent := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
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

	refreshed, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, loaded)
	require.NoError(t, err)
	require.NotEmpty(t, refreshed.Mounts)
	assert.Equal(t, []string{"Shortcuts/New Docs"}, refreshed.Mounts[0].localSkipDirs)
}

// Validates: R-2.8.1, R-4.1.4
func TestProjectionMoveLifecycle_DoesNotPrecreateTargetBeforeRename(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	parent := compiled.Mounts[0]
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldRoot, "kept.txt"), []byte("stateful"), 0o600))

	inventory, err := config.LoadMountInventory()
	require.NoError(t, err)
	localRootResult := reconcileChildMountLocalRoots([]*mountSpec{parent}, inventory, nil)
	assert.False(t, localRootResult.changed)
	assert.NoDirExists(t, newRoot)

	applyChildProjectionMoves(compiled, nil)
	validateCompiledChildMountRoots(compiled, nil)

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
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
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
func TestApplyChildProjectionMoves_MissingSourceAndTargetStaysUnavailable(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(parentSyncRoot, 0o700))

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.NoDirExists(t, newRoot)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalProjectionUnavailable)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, []string{"Shortcuts/Old Docs"}, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateUnavailable, updated.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionUnavailable, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_TargetAlreadyInPlaceClearsReservation(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Shortcuts/New Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(newRoot, 0o700))

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.DirExists(t, newRoot)
	assert.Empty(t, compiled.Skipped)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Empty(t, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, updated.State)
	assert.Empty(t, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_SymlinkedAncestorConflictsChild(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Old Docs", "Linked/New Docs")
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(parentSyncRoot, "Linked")))

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalProjectionConflict)
	assert.NoDirExists(t, filepath.Join(outside, "New Docs"))

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, config.MountStateConflict, updated.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionConflict, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_CaseOnlyRenameOnCaseInsensitiveFilesystem(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Shortcuts/Docs", "Shortcuts/docs")
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	if _, err := os.Stat(newRoot); err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
		t.Skip("case-sensitive filesystem does not expose case-only rename dual-stat behavior")
	}

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.DirExists(t, newRoot)
	assert.Empty(t, compiled.Skipped)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Empty(t, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, updated.State)
	assert.Empty(t, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_TargetSymlinkAncestorConflictsAndSkips(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, "Old Docs", "Shortcuts/New Docs")
	oldRoot := filepath.Join(parentSyncRoot, "Old Docs")
	escapeRoot := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.MkdirAll(escapeRoot, 0o700))
	require.NoError(t, os.Symlink(escapeRoot, filepath.Join(parentSyncRoot, "Shortcuts")))

	applyChildProjectionMoves(compiled, nil)

	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalProjectionConflict)
	assert.DirExists(t, oldRoot)
	assert.NoDirExists(t, filepath.Join(escapeRoot, "New Docs"))

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, config.MountStateConflict, updated.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionConflict, updated.StateReason)
	assert.Equal(t, []string{"Old Docs"}, updated.ReservedLocalPaths)
}
