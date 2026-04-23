package multisync

import (
	"context"
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
		mountID:       mountID("personal:owner@example.com"),
		canonicalID:   driveid.MustCanonicalID("personal:owner@example.com"),
		remoteDriveID: driveid.New("parent-drive"),
	}
}

func testChildRecord(parentID mountID, bindingID, relativePath string) config.MountRecord {
	return config.MountRecord{
		MountID:           config.ChildMountID(parentID.String(), bindingID),
		ParentMountID:     parentID.String(),
		BindingItemID:     bindingID,
		DisplayName:       "Shortcut",
		RelativeLocalPath: relativePath,
		RemoteDriveID:     "remote-drive",
		RemoteRootItemID:  "remote-root",
	}
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FullEnumerationUpdatesBindingInPlace(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	orchestrator := &Orchestrator{}
	result, err := orchestrator.reconcileParentMountDelta(
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
		config.ParentDiscoveryState{ParentMountID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)

	record := inventory.Mounts[existing.MountID]
	assert.Equal(t, existing.MountID, record.MountID)
	assert.Equal(t, "Docs Renamed", record.DisplayName)
	assert.Equal(t, "Shortcuts/Docs Renamed", record.RelativeLocalPath)
	assert.Equal(t, config.DiscoveryModeDelta, inventory.Parents[parent.mountID.String()].DiscoveryMode)
	assert.Equal(t, "delta-token-1", inventory.Parents[parent.mountID.String()].DeltaLink)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_FullEnumerationRemovesMissingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-old", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	orchestrator := &Orchestrator{}
	result, err := orchestrator.reconcileParentMountDelta(
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
		config.ParentDiscoveryState{ParentMountID: parent.mountID.String()},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Equal(t, []string{existing.MountID}, result.removedMountIDs)
	assert.NotContains(t, inventory.Mounts, existing.MountID)

	newMountID := config.ChildMountID(parent.mountID.String(), "binding-new")
	record := inventory.Mounts[newMountID]
	assert.Equal(t, "Team Docs", record.DisplayName)
	assert.Equal(t, "Shared/Team Docs", record.RelativeLocalPath)
	assert.Equal(t, "remote-next", record.RemoteDriveID)
	assert.Equal(t, "remote-item-next", record.RemoteRootItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountDelta_GoneResetsTokenWithoutRemovingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-1", "Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	orchestrator := &Orchestrator{}
	result, err := orchestrator.reconcileParentMountDelta(
		t.Context(),
		inventory,
		parent,
		&driveopsSessionView{meta: &fakeShortcutDiscoveryClient{
			deltaAllErr: graph.ErrGone,
		}},
		"",
		config.ParentDiscoveryState{
			ParentMountID: parent.mountID.String(),
			DeltaLink:     "stale-token",
			DiscoveryMode: config.DiscoveryModeDelta,
		},
		false,
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)
	assert.Contains(t, inventory.Mounts, existing.MountID)
	assert.Empty(t, inventory.Parents[parent.mountID.String()].DeltaLink)
	assert.Equal(t, config.DiscoveryModeDelta, inventory.Parents[parent.mountID.String()].DiscoveryMode)
}

// Validates: R-2.8.1, R-4.1.4
func TestReconcileParentMountByListing_PositiveOnlyKeepsExistingBindings(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	existing := testChildRecord(parent.mountID, "binding-existing", "Existing Shortcut")
	inventory := config.DefaultMountInventory()
	inventory.Mounts[existing.MountID] = existing

	orchestrator := &Orchestrator{}
	result, err := orchestrator.reconcileParentMountByListing(
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
		config.ParentDiscoveryState{ParentMountID: parent.mountID.String()},
	)
	require.NoError(t, err)
	assert.True(t, result.changed)
	assert.Empty(t, result.removedMountIDs)
	assert.Contains(t, inventory.Mounts, existing.MountID)

	newMountID := config.ChildMountID(parent.mountID.String(), "binding-new")
	record := inventory.Mounts[newMountID]
	assert.Equal(t, "Shared/Docs", record.RelativeLocalPath)
	assert.Equal(t, config.DiscoveryModeEnumerate, inventory.Parents[parent.mountID.String()].DiscoveryMode)
	assert.Empty(t, inventory.Parents[parent.mountID.String()].DeltaLink)
}
