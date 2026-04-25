package multisync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

const (
	shortcutOldDocsPath = "Shortcuts/Old Docs"
	shortcutNewDocsPath = "Shortcuts/New Docs"
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
	compiled, record, parentSyncRoot, parent := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
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
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
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
func TestApplyChildProjectionMoves_SkipsDirtyChildDroppedAfterPersistFailure(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldRoot, "kept.txt"), []byte("stateful"), 0o600))

	applyInventoryPersistFailure(compiled, []string{record.MountID}, assert.AnError)
	require.Empty(t, compiled.ProjectionMoves)
	changed := applyChildProjectionMoves(compiled, nil)

	assert.False(t, changed)
	assert.DirExists(t, oldRoot)
	assert.FileExists(t, filepath.Join(oldRoot, "kept.txt"))
	assert.NoDirExists(t, newRoot)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "unpersisted lifecycle state")

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, []string{"Shortcuts/Old Docs"}, updated.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, updated.State)
	assert.Empty(t, updated.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_PersistFailureStillMovesCleanChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.SyncRoot = filepath.Join(t.TempDir(), "parent")

	dirty := testMountRecordForParent(&parent)
	dirty.RelativeLocalPath = "Shortcuts/Dirty New"
	dirty.ReservedLocalPaths = []string{"Shortcuts/Dirty Old"}

	clean := dirty
	clean.MountID = "child-clean"
	clean.BindingItemID = "binding-child-clean"
	clean.LocalAlias = "Clean New"
	clean.RelativeLocalPath = "Shortcuts/Clean New"
	clean.ReservedLocalPaths = []string{"Shortcuts/Clean Old"}
	clean.RemoteDriveID = "remote-drive-clean"
	clean.RemoteItemID = "remote-root-clean"

	inventory := config.DefaultMountInventory()
	inventory.Mounts[dirty.MountID] = dirty
	inventory.Mounts[clean.MountID] = clean
	require.NoError(t, config.SaveMountInventory(inventory))

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, inventory)
	require.NoError(t, err)
	require.Len(t, compiled.ProjectionMoves, 2)

	dirtyOldRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Dirty Old")
	dirtyNewRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Dirty New")
	cleanOldRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Clean Old")
	cleanNewRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Clean New")
	require.NoError(t, os.MkdirAll(dirtyOldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dirtyOldRoot, "kept.txt"), []byte("dirty"), 0o600))
	require.NoError(t, os.MkdirAll(cleanOldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cleanOldRoot, "kept.txt"), []byte("clean"), 0o600))

	applyInventoryPersistFailure(compiled, []string{dirty.MountID}, assert.AnError)
	require.Len(t, compiled.ProjectionMoves, 1)
	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.DirExists(t, dirtyOldRoot)
	assert.FileExists(t, filepath.Join(dirtyOldRoot, "kept.txt"))
	assert.NoDirExists(t, dirtyNewRoot)
	assert.NoDirExists(t, cleanOldRoot)
	assert.FileExists(t, filepath.Join(cleanNewRoot, "kept.txt"))
	require.Len(t, compiled.Mounts, 2)
	require.Len(t, compiled.Skipped, 1)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	assert.Equal(t, []string{"Shortcuts/Dirty Old"}, loaded.Mounts[dirty.MountID].ReservedLocalPaths)
	assert.Empty(t, loaded.Mounts[clean.MountID].ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_TargetConflictMarksChildConflictAndSkips(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.MkdirAll(newRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldRoot, "file.txt"), []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(newRoot, "file.txt"), []byte("new"), 0o600))

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
func TestApplyChildProjectionMoves_TargetEmptyAutoResolves(t *testing.T) {
	compiled, record, parentSyncRoot, parent := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldRoot, "kept.txt"), []byte("stateful"), 0o600))
	require.NoError(t, os.MkdirAll(newRoot, 0o700))

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.NoDirExists(t, oldRoot)
	assert.FileExists(t, filepath.Join(newRoot, "kept.txt"))
	assert.Empty(t, compiled.Skipped)

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
func TestApplyChildProjectionMoves_MatchingTreesAutoResolve(t *testing.T) {
	compiled, record, parentSyncRoot, parent := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	for _, root := range []string{oldRoot, newRoot} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "nested", "empty"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "kept.txt"), []byte("same"), 0o600))
	}

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.NoDirExists(t, oldRoot)
	assert.FileExists(t, filepath.Join(newRoot, "nested", "kept.txt"))
	assert.Empty(t, compiled.Skipped)

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
func TestApplyChildProjectionMoves_SymlinkInExistingTreeConflicts(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
	oldRoot := filepath.Join(parentSyncRoot, "Shortcuts", "Old Docs")
	newRoot := filepath.Join(parentSyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(oldRoot, 0o700))
	require.NoError(t, os.MkdirAll(newRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(newRoot, "file.txt"), []byte("target"), 0o600))
	if symlinkErr := os.Symlink(filepath.Join(newRoot, "file.txt"), filepath.Join(oldRoot, "link")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalProjectionConflict)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	updated := loaded.Mounts[record.MountID]
	assert.Equal(t, config.MountStateConflict, updated.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionConflict, updated.StateReason)
	assert.Equal(t, []string{"Shortcuts/Old Docs"}, updated.ReservedLocalPaths)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildProjectionMoves_MissingSourceAndTargetStaysUnavailable(t *testing.T) {
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
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
	compiled, record, parentSyncRoot, _ := compiledChildProjectionMoveFixture(t, shortcutOldDocsPath, shortcutNewDocsPath)
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
func TestApplyChildProjectionMoves_RetriesSkippedLifecycleRecoveryMove(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.SyncRoot = filepath.Join(t.TempDir(), "parent")

	record := testMountRecordForParent(&parent)
	record.RelativeLocalPath = shortcutNewDocsPath
	record.ReservedLocalPaths = []string{shortcutOldDocsPath}
	record.State = config.MountStateConflict
	record.StateReason = config.MountStateReasonLocalProjectionConflict

	inventory := config.DefaultMountInventory()
	inventory.Mounts[record.MountID] = record
	require.NoError(t, config.SaveMountInventory(inventory))

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, inventory)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	require.Len(t, compiled.ProjectionMoves, 1)

	newRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "New Docs")
	require.NoError(t, os.MkdirAll(newRoot, 0o700))

	changed := applyChildProjectionMoves(compiled, nil)

	assert.True(t, changed)
	assert.DirExists(t, newRoot)

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
