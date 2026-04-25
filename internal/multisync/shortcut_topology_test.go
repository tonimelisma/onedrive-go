package multisync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func testParentMountSpec() *mountSpec {
	return &mountSpec{
		mountID:             mountID("personal:owner@example.com"),
		canonicalID:         driveid.MustCanonicalID("personal:owner@example.com"),
		tokenOwnerCanonical: driveid.MustCanonicalID("personal:owner@example.com"),
		remoteDriveID:       driveid.New("parent-drive"),
	}
}

func testChildRecord(namespaceID mountID, bindingID, relativePath string) config.MountRecord {
	return config.MountRecord{
		MountID:             config.ChildMountID(namespaceID.String(), bindingID),
		NamespaceID:         namespaceID.String(),
		BindingItemID:       bindingID,
		LocalAlias:          "Shortcut",
		RelativeLocalPath:   relativePath,
		TokenOwnerCanonical: "personal:owner@example.com",
		RemoteDriveID:       "remote-drive",
		RemoteItemID:        "remote-root",
		State:               config.MountStateActive,
	}
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_CompleteEnumerationUpdatesAndRemovesBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-old", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing
	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)

	result, err := applyShortcutTopologyBatchToInventory(
		inventory,
		parent,
		existingByBinding,
		syncengine.ShortcutTopologyBatch{
			NamespaceID: parent.mountID.String(),
			Kind:        syncengine.ShortcutTopologyObservationComplete,
			Upserts: []syncengine.ShortcutBindingUpsert{{
				BindingItemID:     "binding-new",
				LocalAlias:        "Team Docs",
				RelativeLocalPath: "Shared/Team Docs",
				RemoteDriveID:     "remote-next",
				RemoteItemID:      "remote-item-next",
				RemoteIsFolder:    true,
				Complete:          true,
			}},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.ElementsMatch(t, []string{existing.MountID, config.ChildMountID(parent.mountID.String(), "binding-new")}, result.dirtyMountIDs)
	assert.Equal(t, config.MountStatePendingRemoval, inventory.Mounts[existing.MountID].State)

	newMountID := config.ChildMountID(parent.mountID.String(), "binding-new")
	record := inventory.Mounts[newMountID]
	assert.Equal(t, "Team Docs", record.LocalAlias)
	assert.Equal(t, "Shared/Team Docs", record.RelativeLocalPath)
	assert.Equal(t, "remote-next", record.RemoteDriveID)
	assert.Equal(t, "remote-item-next", record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_RemoteDeleteMarksPendingRemoval(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing
	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)

	result, err := applyShortcutTopologyBatchToInventory(
		inventory,
		parent,
		existingByBinding,
		syncengine.ShortcutTopologyBatch{
			NamespaceID: parent.mountID.String(),
			Kind:        syncengine.ShortcutTopologyObservationIncremental,
			Deletes: []syncengine.ShortcutBindingDelete{{
				BindingItemID: "binding-1",
			}},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Equal(t, []string{existing.MountID}, result.dirtyMountIDs)

	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, config.MountStatePendingRemoval, record.State)
	assert.Equal(t, config.MountStateReasonShortcutRemoved, record.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_SamePathReplacementDefersNewBinding(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-old", "Shortcut")
	existing.State = config.MountStatePendingRemoval
	existing.StateReason = config.MountStateReasonShortcutRemoved
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing
	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)

	result, err := applyShortcutTopologyBatchToInventory(
		inventory,
		parent,
		existingByBinding,
		syncengine.ShortcutTopologyBatch{
			NamespaceID: parent.mountID.String(),
			Kind:        syncengine.ShortcutTopologyObservationIncremental,
			Upserts: []syncengine.ShortcutBindingUpsert{{
				BindingItemID:     "binding-new",
				LocalAlias:        "Shortcut",
				RelativeLocalPath: "Shortcut",
				RemoteDriveID:     "remote-next",
				RemoteItemID:      "remote-item-next",
				RemoteIsFolder:    true,
				Complete:          true,
			}},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Equal(t, []string{existing.MountID}, result.dirtyMountIDs)
	assert.NotContains(t, inventory.Mounts, config.ChildMountID(parent.mountID.String(), "binding-new"))

	key := deferredShortcutBindingKey(parent.mountID.String(), "Shortcut", "binding-new")
	deferred := inventory.DeferredShortcutBindings[key]
	assert.Equal(t, "remote-next", deferred.RemoteDriveID)
	assert.Equal(t, "remote-item-next", deferred.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_UnavailableBindingPersistsUnavailableRecord(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	inventory := config.DefaultMountInventory()
	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)

	result, err := applyShortcutTopologyBatchToInventory(
		inventory,
		parent,
		existingByBinding,
		syncengine.ShortcutTopologyBatch{
			NamespaceID: parent.mountID.String(),
			Kind:        syncengine.ShortcutTopologyObservationIncremental,
			Unavailable: []syncengine.ShortcutBindingUnavailable{{
				BindingItemID:     "binding-1",
				LocalAlias:        "Shortcut",
				RelativeLocalPath: "Shortcuts/Shortcut",
				RemoteDriveID:     "remote-drive",
				RemoteIsFolder:    true,
				Reason:            "shortcut_binding_unavailable",
			}},
		},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)

	record := inventory.Mounts[config.ChildMountID(parent.mountID.String(), "binding-1")]
	assert.Equal(t, config.MountStateUnavailable, record.State)
	assert.Equal(t, config.MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Equal(t, "Shortcuts/Shortcut", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Empty(t, record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyDurableProjectionConflicts_DuplicateChildrenAllConflict(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	first := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	second := testChildRecord(parent.mountID, "binding-b", "Shortcuts/B")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[first.MountID] = first
	inventory.Mounts[second.MountID] = second

	changed := applyDurableProjectionConflicts(inventory, []*mountSpec{parent})

	assert.True(t, changed.changed)
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[first.MountID].State)
	assert.Equal(t, config.MountStateReasonDuplicateContentRoot, inventory.Mounts[first.MountID].StateReason)
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[second.MountID].State)
	assert.Equal(t, config.MountStateReasonDuplicateContentRoot, inventory.Mounts[second.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyDurableProjectionConflicts_DuplicateChildrenAreNamespaceLocal(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	otherParent := *parent
	otherParent.mountID = mountID("personal:owner@example.com:secondary")
	first := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	second := testChildRecord(otherParent.mountID, "binding-b", "Other/A")
	second.State = config.MountStateConflict
	second.StateReason = config.MountStateReasonDuplicateContentRoot
	inventory := config.DefaultMountInventory()
	inventory.Mounts[first.MountID] = first
	inventory.Mounts[second.MountID] = second

	changed := applyDurableProjectionConflicts(inventory, []*mountSpec{parent, &otherParent})

	assert.True(t, changed.changed)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[first.MountID].State)
	assert.Empty(t, inventory.Mounts[first.MountID].StateReason)
	assert.Equal(t, config.MountStateActive, inventory.Mounts[second.MountID].State)
	assert.Empty(t, inventory.Mounts[second.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyDurableProjectionConflicts_StandaloneContentRootWins(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	standalone := &mountSpec{
		mountID:             mountID("shared:owner@example.com:remote-drive:remote-root"),
		tokenOwnerCanonical: parent.tokenOwnerCanonical,
		remoteDriveID:       driveid.New("remote-drive"),
		remoteRootItemID:    "remote-root",
	}
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[child.MountID] = child

	changed := applyDurableProjectionConflicts(inventory, []*mountSpec{parent, standalone})

	assert.True(t, changed.changed)
	assert.Equal(t, config.MountStateConflict, inventory.Mounts[child.MountID].State)
	assert.Equal(t, config.MountStateReasonExplicitStandaloneContentRoot, inventory.Mounts[child.MountID].StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestFinalizePendingMountRemovals_RecompileReleasesParentSkipDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	record := testMountRecordForParent(&parent)
	record.State = config.MountStatePendingRemoval
	record.StateReason = config.MountStateReasonShortcutRemoved
	inventory := config.DefaultMountInventory()
	inventory.Mounts[record.MountID] = record
	require.NoError(t, config.SaveMountInventory(inventory))

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, inventory)
	require.NoError(t, err)
	require.NotEmpty(t, compiled.Mounts)
	assert.Equal(t, []string{"Shortcuts/Docs"}, compiled.Mounts[0].localSkipDirs)

	finalized, err := finalizePendingMountRemovals([]string{record.MountID}, compiled.Mounts, nil)
	require.NoError(t, err)
	assert.True(t, finalized)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	refreshed, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, loaded)
	require.NoError(t, err)
	require.NotEmpty(t, refreshed.Mounts)
	assert.Empty(t, refreshed.Mounts[0].localSkipDirs)
}

// Validates: R-2.8.1, R-4.1.4
func TestFinalizePendingMountRemovals_DirtyProjectionStaysReserved(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	record := testMountRecordForParent(&parent)
	record.State = config.MountStatePendingRemoval
	record.StateReason = config.MountStateReasonShortcutRemoved
	projectionRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(projectionRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(projectionRoot, "local.txt"), []byte("dirty"), 0o600))

	inventory := config.DefaultMountInventory()
	inventory.Mounts[record.MountID] = record
	require.NoError(t, config.SaveMountInventory(inventory))

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, inventory)
	require.NoError(t, err)
	finalized, err := finalizePendingMountRemovals([]string{record.MountID}, compiled.Mounts, nil)
	require.NoError(t, err)
	assert.False(t, finalized)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	refreshed := loaded.Mounts[record.MountID]
	assert.Equal(t, config.MountStatePendingRemoval, refreshed.State)
	assert.Equal(t, config.MountStateReasonRemovedProjectionDirty, refreshed.StateReason)
}
