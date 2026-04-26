package multisync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func testChildRecord(_ mountID, bindingID, relativePath string) syncengine.ShortcutChildTopology {
	return syncengine.ShortcutChildTopology{
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		LocalAlias:        "Shortcut",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		State:             syncengine.ShortcutChildDesired,
		ProtectedPaths:    []string{relativePath},
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
		State:             shortcutChildStateForRoot(root.State),
		ProtectedPaths:    append([]string(nil), root.ProtectedPaths...),
		Waiting:           shortcutWaitingForTest(root.Waiting),
	}
}

func shortcutChildStateForRoot(state syncengine.ShortcutRootState) syncengine.ShortcutChildTopologyState {
	switch state {
	case "", syncengine.ShortcutRootStateActive:
		return syncengine.ShortcutChildDesired
	case syncengine.ShortcutRootStateRemovedFinalDrain,
		syncengine.ShortcutRootStateRemovedCleanupBlocked:
		return syncengine.ShortcutChildRetiring
	case syncengine.ShortcutRootStateSamePathReplacementWaiting:
		return syncengine.ShortcutChildRetiring
	case syncengine.ShortcutRootStateTargetUnavailable,
		syncengine.ShortcutRootStateBlockedPath,
		syncengine.ShortcutRootStateRenameAmbiguous,
		syncengine.ShortcutRootStateAliasMutationBlocked:
		return syncengine.ShortcutChildBlocked
	default:
		return syncengine.ShortcutChildBlocked
	}
}

func shortcutWaitingForTest(waiting *syncengine.ShortcutRootReplacement) *syncengine.ShortcutChildTopology {
	if waiting == nil {
		return nil
	}
	return &syncengine.ShortcutChildTopology{
		BindingItemID:     waiting.BindingItemID,
		RelativeLocalPath: waiting.RelativeLocalPath,
		LocalAlias:        waiting.LocalAlias,
		RemoteDriveID:     waiting.RemoteDriveID.String(),
		RemoteItemID:      waiting.RemoteItemID,
		RemoteIsFolder:    waiting.RemoteIsFolder,
		State:             syncengine.ShortcutChildWaitingReplacement,
		ProtectedPaths:    []string{waiting.RelativeLocalPath},
	}
}

func seedShortcutChildTopology(
	orch *Orchestrator,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildTopology,
) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return
	}
	orch.storeParentShortcutTopology(mountID(parent.CanonicalID.String()), syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.CanonicalID.String(),
		Children:    []syncengine.ShortcutChildTopology{*child},
	})
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentShortcutTopology_StoresPublicationInMemory(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})

	changed := orch.receiveParentShortcutTopology(parent, syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			State:             syncengine.ShortcutChildDesired,
			ProtectedPaths:    []string{"Shared/Docs"},
		}},
	})

	assert.True(t, changed)
	publication := orch.parentShortcutTopologyFor(parent.mountID)
	require.Len(t, publication.Children, 1)
	assert.Equal(t, syncengine.ShortcutChildDesired, publication.Children[0].State)
	assert.Equal(t, "Shared/Docs", publication.Children[0].RelativeLocalPath)
	assert.Equal(t, "remote-drive-0001", publication.Children[0].RemoteDriveID)
	assert.Equal(t, "remote-root", publication.Children[0].RemoteItemID)
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestReceiveParentShortcutTopology_EmptyPublicationClearsCachedChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.storeParentShortcutTopology(parent.mountID, syncengine.ShortcutChildTopologyPublication{
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

	changed := orch.receiveParentShortcutTopology(parent, syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
	})

	assert.True(t, changed)
	publication := orch.parentShortcutTopologyFor(parent.mountID)
	assert.Empty(t, publication.Children)
}

// Validates: R-2.8.1, R-4.1.4
func TestParentShortcutTopologyCache_ClonesPublication(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	publication := syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shortcuts/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			State:             syncengine.ShortcutChildDesired,
			ProtectedPaths:    []string{"Shortcuts/Docs"},
			Waiting: &syncengine.ShortcutChildTopology{
				BindingItemID:     "binding-next",
				RelativeLocalPath: "Shortcuts/Docs",
				ProtectedPaths:    []string{"Shortcuts/Docs"},
			},
		}},
	}

	changed := orch.receiveParentShortcutTopology(parent, publication)
	require.True(t, changed)
	publication.Children[0].ProtectedPaths[0] = "mutated"
	publication.Children[0].Waiting.ProtectedPaths[0] = "mutated-waiting"

	cached := orch.parentShortcutTopologyFor(parent.mountID)
	require.Len(t, cached.Children, 1)
	assert.Equal(t, []string{"Shortcuts/Docs"}, cached.Children[0].ProtectedPaths)
	require.NotNil(t, cached.Children[0].Waiting)
	assert.Equal(t, []string{"Shortcuts/Docs"}, cached.Children[0].Waiting.ProtectedPaths)

	cached.Children[0].ProtectedPaths[0] = "mutated-cached"
	cachedAgain := orch.parentShortcutTopologyFor(parent.mountID)
	assert.Equal(t, []string{"Shortcuts/Docs"}, cachedAgain.Children[0].ProtectedPaths)
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

	changed := orch.receiveParentShortcutTopology(parent, syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
		Children:    []syncengine.ShortcutChildTopology{topologyForTest(&root)},
	})

	assert.True(t, changed)
	publication := orch.parentShortcutTopologyFor(parent.mountID)
	require.Len(t, publication.Children, 1)
	assert.Equal(t, "binding-old", publication.Children[0].BindingItemID)
	assert.Equal(t, syncengine.ShortcutChildRetiring, publication.Children[0].State)
	require.NotNil(t, publication.Children[0].Waiting)
	assert.Equal(t, "binding-new", publication.Children[0].Waiting.BindingItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestCompileRuntimeMountsFromParentTopology_DuplicateChildrenAllSkip(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := testParentRoot(parent, "binding-a", "Shortcuts/A")
	second := testParentRoot(parent, "binding-b", "Shortcuts/B")
	orch.storeParentShortcutTopology(parent.mountID, syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{
			topologyForTest(&first),
			topologyForTest(&second),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent},
		orch.parentShortcutTopologiesFor([]*mountSpec{parent}),
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
	orch.storeParentShortcutTopology(parent.mountID, syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildTopology{
			topologyForTest(&root),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent, standalone},
		orch.parentShortcutTopologiesFor([]*mountSpec{parent, standalone}),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, compiled.Mounts, 2)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "standalone mount")
}
