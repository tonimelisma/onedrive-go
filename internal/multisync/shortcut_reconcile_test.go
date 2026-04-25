package multisync

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type fakeShortcutDiscoveryClient struct {
	deltaAllItems      []graph.Item
	deltaAllToken      string
	deltaAllErr        error
	deltaFolderItems   []graph.Item
	deltaFolderToken   string
	deltaFolderErr     error
	itemsByID          map[string]*graph.Item
	childrenByParentID map[string][]graph.Item
}

func (f *fakeShortcutDiscoveryClient) DeltaAll(_ context.Context, _ string, _ string) ([]graph.Item, string, error) {
	return append([]graph.Item(nil), f.deltaAllItems...), f.deltaAllToken, f.deltaAllErr
}

func (f *fakeShortcutDiscoveryClient) DeltaFolderAll(_ context.Context, _ string, _ string, _ string) ([]graph.Item, string, error) {
	return append([]graph.Item(nil), f.deltaFolderItems...), f.deltaFolderToken, f.deltaFolderErr
}

func (f *fakeShortcutDiscoveryClient) GetItem(_ context.Context, _ string, itemID string) (*graph.Item, error) {
	if item, ok := f.itemsByID[itemID]; ok {
		itemCopy := *item
		return &itemCopy, nil
	}

	return nil, graph.ErrNotFound
}

func (f *fakeShortcutDiscoveryClient) ListChildren(_ context.Context, _ string, parentID string) ([]graph.Item, error) {
	return append([]graph.Item(nil), f.childrenByParentID[parentID]...), nil
}

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
func TestNamespaceRuntime_ReconcileWithoutParentsKeepsInventoryForSkippedChildren(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))
	orphan := testChildRecord(mountID("personal:missing@example.com"), "binding-orphan", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[orphan.MountID] = orphan
	require.NoError(t, config.SaveMountInventory(inventory))

	result, err := (&namespaceRuntime{}).reconcile(t.Context(), nil)

	require.NoError(t, err)
	require.NotNil(t, result.inventory)
	assert.Contains(t, result.inventory.Mounts, orphan.MountID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FullEnumerationUpdatesBindingInPlace(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	existing.LocalRootMaterialized = true
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:            "binding-1",
				Name:          "Docs Renamed",
				ParentPath:    "Shortcuts",
				IsFolder:      true,
				RemoteDriveID: "remote-drive",
				RemoteItemID:  "remote-root",
			}},
			deltaAllToken: "delta-token-1",
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)

	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, existing.MountID, record.MountID)
	assert.Equal(t, "Docs Renamed", record.LocalAlias)
	assert.Equal(t, "Shortcuts/Docs Renamed", record.RelativeLocalPath)
	assert.Equal(t, []string{"Shortcut"}, record.ReservedLocalPaths)
	assert.False(t, record.LocalRootMaterialized)
	assert.Equal(t, config.DiscoveryModeDelta, inventory.Namespaces[parent.mountID.String()].DiscoveryMode)
	assert.Equal(t, "delta-token-1", inventory.Namespaces[parent.mountID.String()].DeltaLink)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_DetectsRemoteFolderPlaceholder(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	inventory := config.DefaultMountInventory()

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:             "binding-1",
				Name:           "Shortcut",
				ParentPath:     "Shortcuts",
				IsFolder:       false,
				RemoteIsFolder: true,
				RemoteDriveID:  "remote-drive",
				RemoteItemID:   "remote-root",
			}},
			deltaAllToken: "delta-token-1",
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)

	record := inventory.Mounts[config.ChildMountID(parent.mountID.String(), "binding-1")]
	assert.Equal(t, "Shortcuts/Shortcut", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Equal(t, "remote-root", record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FullEnumerationRemovesMissingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-old", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:            "binding-new",
				Name:          "Team Docs",
				ParentPath:    "Shared",
				IsFolder:      true,
				RemoteDriveID: "remote-next",
				RemoteItemID:  "remote-item-next",
			}},
			deltaAllToken: "delta-token-2",
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Equal(t, []string{existing.MountID}, result.removedMountIDs)
	assert.Equal(t, config.MountStatePendingRemoval, inventory.Mounts[existing.MountID].State)

	newMountID := config.ChildMountID(parent.mountID.String(), "binding-new")
	record := inventory.Mounts[newMountID]
	assert.Equal(t, "Team Docs", record.LocalAlias)
	assert.Equal(t, "Shared/Team Docs", record.RelativeLocalPath)
	assert.Equal(t, "remote-next", record.RemoteDriveID)
	assert.Equal(t, "remote-item-next", record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_GoneResetsTokenWithoutRemovingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllErr: graph.ErrGone,
		}},
		"",
		config.NamespaceDiscoveryState{
			NamespaceID:   parent.mountID.String(),
			DeltaLink:     "stale-token",
			DiscoveryMode: config.DiscoveryModeDelta,
		},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)
	assert.Contains(t, inventory.Mounts, existing.MountID)
	assert.Empty(t, inventory.Namespaces[parent.mountID.String()].DeltaLink)
	assert.Equal(t, config.DiscoveryModeDelta, inventory.Namespaces[parent.mountID.String()].DiscoveryMode)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_KnownShortcutRefreshFailureMarksUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:         "binding-1",
				Name:       "Shortcut",
				ParentPath: "Shortcuts",
				IsFolder:   true,
			}},
			deltaAllToken: "delta-token-1",
			itemsByID:     map[string]*graph.Item{},
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)

	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, config.MountStateUnavailable, record.State)
	assert.Equal(t, config.MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Equal(t, existing.RemoteDriveID, record.RemoteDriveID)
	assert.Equal(t, existing.RemoteItemID, record.RemoteItemID)
	assert.Equal(t, "Shortcuts/Shortcut", record.RelativeLocalPath)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_SuccessfulRefreshReactivatesUnavailableShortcut(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	existing.State = config.MountStateUnavailable
	existing.StateReason = config.MountStateReasonShortcutBindingUnavailable
	existing.RemoteDriveID = ""
	existing.RemoteItemID = ""
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:            "binding-1",
				Name:          "Shortcut",
				ParentPath:    "Shortcuts",
				IsFolder:      true,
				RemoteDriveID: "remote-drive-next",
				RemoteItemID:  "remote-root-next",
			}},
			deltaAllToken: "delta-token-1",
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)

	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, config.MountStateActive, record.State)
	assert.Empty(t, record.StateReason)
	assert.Equal(t, "remote-drive-next", record.RemoteDriveID)
	assert.Equal(t, "remote-root-next", record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestRetryUnavailableShortcutBindings_ReactivatesWithoutDeltaEvent(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcuts/Shortcut")
	existing.State = config.MountStateUnavailable
	existing.StateReason = config.MountStateReasonShortcutBindingUnavailable
	existing.RemoteDriveID = ""
	existing.RemoteItemID = ""
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result := namespaceRuntime.retryUnavailableShortcutBindings(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			itemsByID: map[string]*graph.Item{
				"binding-1": {
					ID:            "binding-1",
					Name:          "Shortcut",
					ParentPath:    "Shortcuts",
					IsFolder:      true,
					RemoteDriveID: "remote-drive-next",
					RemoteItemID:  "remote-root-next",
				},
			},
		}},
		"",
	)

	assert.True(t, result.changed)
	assert.Equal(t, []string{existing.MountID}, result.dirtyMountIDs)
	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, config.MountStateActive, record.State)
	assert.Empty(t, record.StateReason)
	assert.Equal(t, "remote-drive-next", record.RemoteDriveID)
	assert.Equal(t, "remote-root-next", record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestRetryUnavailableShortcutBindings_FailedRefreshKeepsUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcuts/Shortcut")
	existing.State = config.MountStateUnavailable
	existing.StateReason = config.MountStateReasonShortcutBindingUnavailable
	existing.RemoteDriveID = ""
	existing.RemoteItemID = ""
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result := namespaceRuntime.retryUnavailableShortcutBindings(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{}},
		"",
	)

	assert.False(t, result.changed)
	assert.Empty(t, result.dirtyMountIDs)
	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, config.MountStateUnavailable, record.State)
	assert.Equal(t, config.MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Empty(t, record.RemoteDriveID)
	assert.Empty(t, record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FirstSeenPartialShortcutPersistsUnavailableWhenPathKnown(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	inventory := config.DefaultMountInventory()

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:            "binding-1",
				Name:          "Shortcut",
				ParentPath:    "Shortcuts",
				IsFolder:      true,
				RemoteDriveID: "remote-drive",
			}},
			deltaAllToken: "delta-token-1",
			itemsByID:     map[string]*graph.Item{},
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)

	mountID := config.ChildMountID(parent.mountID.String(), "binding-1")
	record := inventory.Mounts[mountID]
	assert.Equal(t, config.MountStateUnavailable, record.State)
	assert.Equal(t, config.MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Equal(t, "Shortcuts/Shortcut", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Empty(t, record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FirstSeenRemoteFolderRefreshFailurePersistsUnavailable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	inventory := config.DefaultMountInventory()

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllItems: []graph.Item{{
				ID:             "binding-1",
				Name:           "Shortcut",
				ParentPath:     "Shortcuts",
				RemoteIsFolder: true,
				RemoteDriveID:  "remote-drive",
			}},
			deltaAllToken: "delta-token-1",
			itemsByID:     map[string]*graph.Item{},
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)

	mountID := config.ChildMountID(parent.mountID.String(), "binding-1")
	record := inventory.Mounts[mountID]
	assert.Equal(t, config.MountStateUnavailable, record.State)
	assert.Equal(t, config.MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Equal(t, "Shortcuts/Shortcut", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountByListing_PositiveOnlyKeepsExistingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-existing", "Existing Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	namespaceRuntime := &namespaceRuntime{}
	result, err := namespaceRuntime.reconcileNamespaceMountByListing(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			itemsByID: map[string]*graph.Item{
				"folder-1": {
					ID:         "folder-1",
					Name:       "Shared",
					ParentPath: "",
					IsFolder:   true,
				},
			},
			childrenByParentID: map[string][]graph.Item{
				"root": {{
					ID:       "folder-1",
					Name:     "Shared",
					IsFolder: true,
				}},
				"folder-1": {{
					ID:            "binding-new",
					Name:          "Docs",
					ParentPath:    "Shared",
					IsFolder:      true,
					RemoteDriveID: "remote-new",
					RemoteItemID:  "item-new",
				}},
			},
		}},
		"",
		config.NamespaceDiscoveryState{NamespaceID: parent.mountID.String()},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)
	assert.Contains(t, inventory.Mounts, existing.MountID)

	newMountID := config.ChildMountID(parent.mountID.String(), "binding-new")
	record := inventory.Mounts[newMountID]
	assert.Equal(t, "Shared/Docs", record.RelativeLocalPath)
	assert.Equal(t, config.DiscoveryModeEnumerate, inventory.Namespaces[parent.mountID.String()].DiscoveryMode)
	assert.Empty(t, inventory.Namespaces[parent.mountID.String()].DeltaLink)
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

	finalized, err := finalizePendingMountRemovals([]string{record.MountID})
	require.NoError(t, err)
	assert.True(t, finalized)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	refreshed, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, loaded)
	require.NoError(t, err)
	require.NotEmpty(t, refreshed.Mounts)
	assert.Empty(t, refreshed.Mounts[0].localSkipDirs)
}
