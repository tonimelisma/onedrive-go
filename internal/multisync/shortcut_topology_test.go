package multisync

import (
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
		projectionKind:      MountProjectionStandalone,
		canonicalID:         driveid.MustCanonicalID("personal:owner@example.com"),
		tokenOwnerCanonical: driveid.MustCanonicalID("personal:owner@example.com"),
		remoteDriveID:       driveid.New("parent-drive"),
		syncRoot:            "/tmp/parent",
	}
}

func testParentRoot(parent *mountSpec, bindingID, relativePath string) syncengine.ShortcutRootRecord {
	return syncengine.ShortcutRootRecord{
		NamespaceID:       parent.mountID.String(),
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		LocalAlias:        "Shortcut",
		RemoteDriveID:     driveid.New("remote-drive"),
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		State:             syncengine.ShortcutRootStateActive,
		ProtectedPaths:    []string{relativePath},
	}
}

func testChildRecord(namespaceID mountID, bindingID, relativePath string) childTopologyRecord {
	return childTopologyRecord{
		mountID:             config.ChildMountID(namespaceID.String(), bindingID),
		namespaceID:         namespaceID.String(),
		bindingItemID:       bindingID,
		localAlias:          "Shortcut",
		relativeLocalPath:   relativePath,
		tokenOwnerCanonical: "personal:owner@example.com",
		remoteDriveID:       "remote-drive",
		remoteItemID:        "remote-root",
		state:               childTopologyStateActive,
	}
}

func topologyForTest(root *syncengine.ShortcutRootRecord) syncengine.ShortcutChildTopology {
	if root == nil {
		return syncengine.ShortcutChildTopology{}
	}
	return syncengine.ShortcutChildTopology{
		BindingItemID:     root.BindingItemID,
		RelativeLocalPath: root.RelativeLocalPath,
		LocalAlias:        root.LocalAlias,
		RemoteDriveID:     root.RemoteDriveID.String(),
		RemoteItemID:      root.RemoteItemID,
		RemoteIsFolder:    root.RemoteIsFolder,
		State:             syncengine.ShortcutChildDesired,
		ProtectedPaths:    append([]string(nil), root.ProtectedPaths...),
	}
}

func seedShortcutChildTopology(orch *Orchestrator, parent *StandaloneMountConfig, record *childTopologyRecord) {
	if parent == nil || record == nil {
		return
	}
	orch.storeShortcutChildTopology(mountID(parent.CanonicalID.String()), syncengine.ShortcutChildTopologySnapshot{
		NamespaceID: parent.CanonicalID.String(),
		Children: []syncengine.ShortcutChildTopology{{
			BindingItemID:     record.bindingItemID,
			RelativeLocalPath: record.relativeLocalPath,
			LocalAlias:        record.localAlias,
			RemoteDriveID:     record.remoteDriveID,
			RemoteItemID:      record.remoteItemID,
			RemoteIsFolder:    true,
			State:             shortcutChildStateForTest(record),
			ProtectedPaths:    append([]string{record.relativeLocalPath}, record.reservedLocalPaths...),
		}},
	})
}

func shortcutChildStateForTest(record *childTopologyRecord) syncengine.ShortcutChildTopologyState {
	if record == nil {
		return syncengine.ShortcutChildBlocked
	}
	if record.state == childTopologyStatePendingRemoval &&
		record.stateReason == childTopologyStateReasonShortcutRemoved {
		return syncengine.ShortcutChildRetiring
	}
	if record.state == childTopologyStateUnavailable ||
		record.state == childTopologyStateConflict {
		return syncengine.ShortcutChildBlocked
	}
	return syncengine.ShortcutChildDesired
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_StoresParentDeclaredChildrenInMemory(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})

	changed := orch.applyShortcutTopologyBatch(parent, syncengine.ShortcutTopologyBatch{
		NamespaceID: parent.mountID.String(),
		Kind:        syncengine.ShortcutTopologyObservationComplete,
		ParentRoots: []syncengine.ShortcutRootRecord{{
			NamespaceID:       parent.mountID.String(),
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("remote-drive-0001"),
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			State:             syncengine.ShortcutRootStateActive,
			ProtectedPaths:    []string{"Shared/Docs"},
		}},
	})

	assert.True(t, changed)
	topology := orch.transientShortcutTopology([]*mountSpec{parent})
	record := topology.mounts[config.ChildMountID(parent.mountID.String(), "binding-1")]
	assert.Equal(t, childTopologyStateActive, record.state)
	assert.Equal(t, "Shared/Docs", record.relativeLocalPath)
	assert.Equal(t, "remote-drive-0001", record.remoteDriveID)
	assert.Equal(t, "remote-root", record.remoteItemID)
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestApplyShortcutTopologyBatch_EmptyCompleteClearsCachedChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.storeShortcutChildTopology(parent.mountID, syncengine.ShortcutChildTopologySnapshot{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcut",
			LocalAlias:        "Shortcut",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			State:             syncengine.ShortcutChildDesired,
		}},
	})

	changed := orch.applyShortcutTopologyBatch(parent, syncengine.ShortcutTopologyBatch{
		NamespaceID: parent.mountID.String(),
		Kind:        syncengine.ShortcutTopologyObservationComplete,
	})

	assert.True(t, changed)
	topology := orch.transientShortcutTopology([]*mountSpec{parent})
	assert.Empty(t, topology.mounts)
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestParentWaitingReplacementDoesNotCreateNewChild(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	root := testParentRoot(parent, "binding-old", "Shortcut")
	root.State = syncengine.ShortcutRootStateSamePathReplacementWaiting
	root.Waiting = &syncengine.ShortcutRootReplacement{
		BindingItemID:     "binding-new",
		RelativeLocalPath: "Shortcut",
		LocalAlias:        "Shortcut",
		RemoteDriveID:     driveid.New("remote-next-0001"),
		RemoteItemID:      "remote-item-next",
		RemoteIsFolder:    true,
	}

	changed := orch.applyShortcutTopologyBatch(parent, syncengine.ShortcutTopologyBatch{
		NamespaceID: parent.mountID.String(),
		Kind:        syncengine.ShortcutTopologyObservationComplete,
		ParentRoots: []syncengine.ShortcutRootRecord{root},
	})

	assert.True(t, changed)
	topology := orch.transientShortcutTopology([]*mountSpec{parent})
	assert.Contains(t, topology.mounts, config.ChildMountID(parent.mountID.String(), "binding-old"))
	assert.NotContains(t, topology.mounts, config.ChildMountID(parent.mountID.String(), "binding-new"))
	assert.Equal(
		t,
		childTopologyStatePendingRemoval,
		topology.mounts[config.ChildMountID(parent.mountID.String(), "binding-old")].state,
	)
}

// Validates: R-2.8.1, R-4.1.4
func TestCompileRuntimeMountsFromParentTopology_DuplicateChildrenAllSkip(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := testParentRoot(parent, "binding-a", "Shortcuts/A")
	second := testParentRoot(parent, "binding-b", "Shortcuts/B")
	orch.storeShortcutChildTopology(parent.mountID, syncengine.ShortcutChildTopologySnapshot{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{
			topologyForTest(&first),
			topologyForTest(&second),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent},
		orch.transientShortcutTopology([]*mountSpec{parent}),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 2)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "content root")
	assert.Contains(t, compiled.Skipped[1].Err.Error(), "content root")
}

// Validates: R-2.8.1, R-4.1.4
func TestCompileRuntimeMountsFromParentTopology_StandaloneContentRootWins(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	standalone := &mountSpec{
		mountID:             mountID("shared:owner@example.com:remote-drive:remote-root"),
		projectionKind:      MountProjectionStandalone,
		tokenOwnerCanonical: parent.tokenOwnerCanonical,
		remoteDriveID:       driveid.New("remote-drive"),
		remoteRootItemID:    "remote-root",
	}
	orch := NewOrchestrator(&OrchestratorConfig{})
	root := testParentRoot(parent, "binding-a", "Shortcuts/A")
	orch.storeShortcutChildTopology(parent.mountID, syncengine.ShortcutChildTopologySnapshot{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{
			topologyForTest(&root),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent, standalone},
		orch.transientShortcutTopology([]*mountSpec{parent, standalone}),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, compiled.Mounts, 2)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "standalone mount")
}
