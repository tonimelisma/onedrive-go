package multisync

import (
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

func testChildRecord(_ mountID, bindingID, relativePath string) syncengine.ShortcutChildRunner {
	return syncengine.ShortcutChildRunner{
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		LocalAlias:        "Shortcut",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		RunnerAction:      syncengine.ShortcutChildActionRun,
	}
}

func topologyForTest(root *syncengine.ShortcutRootRecord) syncengine.ShortcutChildRunner {
	if root == nil {
		return syncengine.ShortcutChildRunner{}
	}
	return syncengine.ShortcutChildRunner{
		BindingItemID:     root.BindingItemID,
		RelativeLocalPath: root.RelativeLocalPath,
		LocalAlias:        root.LocalAlias,
		RemoteDriveID:     root.RemoteDriveID.String(),
		RemoteItemID:      root.RemoteItemID,
		RemoteIsFolder:    root.RemoteIsFolder,
		RunnerAction:      shortcutChildActionForRoot(root.State),
	}
}

func shortcutChildActionForRoot(state syncengine.ShortcutRootState) syncengine.ShortcutChildRunnerAction {
	switch state {
	case "", syncengine.ShortcutRootStateActive:
		return syncengine.ShortcutChildActionRun
	case syncengine.ShortcutRootStateRemovedFinalDrain:
		return syncengine.ShortcutChildActionFinalDrain
	case syncengine.ShortcutRootStateSamePathReplacementWaiting:
		return syncengine.ShortcutChildActionFinalDrain
	case syncengine.ShortcutRootStateTargetUnavailable,
		syncengine.ShortcutRootStateLocalRootUnavailable,
		syncengine.ShortcutRootStateBlockedPath,
		syncengine.ShortcutRootStateRenameAmbiguous,
		syncengine.ShortcutRootStateAliasMutationBlocked,
		syncengine.ShortcutRootStateRemovedReleasePending,
		syncengine.ShortcutRootStateRemovedCleanupBlocked,
		syncengine.ShortcutRootStateRemovedChildCleanupPending,
		syncengine.ShortcutRootStateDuplicateTarget:
		return syncengine.ShortcutChildActionSkipParentBlocked
	default:
		return syncengine.ShortcutChildActionSkipParentBlocked
	}
}

func seedShortcutChildRunner(
	orch *Orchestrator,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildRunner,
) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return
	}
	orch.receiveParentRunnerPublication(mountID(parent.CanonicalID.String()), syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.CanonicalID.String(),
		Children:    []syncengine.ShortcutChildRunner{*child},
	})
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_StoresPublicationInMemory(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})

	changed := orch.receiveParentRunnerPublicationFromParent(parent, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			RunnerAction:      syncengine.ShortcutChildActionRun,
		}},
	})

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.Children, 1)
	assert.Equal(t, syncengine.ShortcutChildActionRun, publication.Children[0].RunnerAction)
	assert.Equal(t, "Shared/Docs", publication.Children[0].RelativeLocalPath)
	assert.Equal(t, "remote-drive-0001", publication.Children[0].RemoteDriveID)
	assert.Equal(t, "remote-root", publication.Children[0].RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_EquivalentPublicationIsStable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			RunnerAction:      syncengine.ShortcutChildActionRun,
		}},
	}
	equivalent := syncengine.ShortcutChildRunnerPublication{
		NamespaceID:     parent.mountID.String(),
		CleanupRequests: []syncengine.ShortcutChildArtifactCleanupRequest{},
		Children: []syncengine.ShortcutChildRunner{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			RunnerAction:      syncengine.ShortcutChildActionRun,
		}},
	}

	assert.True(t, orch.receiveParentRunnerPublicationFromParent(parent, first))
	assert.False(t, orch.receiveParentRunnerPublicationFromParent(parent, equivalent))
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_EmptyPublicationClearsCachedChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcut",
			LocalAlias:        "Shortcut",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
		}},
	})

	changed := orch.receiveParentRunnerPublicationFromParent(parent, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
	})

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	assert.Empty(t, publication.Children)
}

// Validates: R-2.8.1, R-4.1.4
func TestParentRunnerPublicationCache_ClonesPublication(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	identity := &syncengine.ShortcutRootIdentity{Device: 1, Inode: 2}
	publication := syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shortcuts/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
			LocalRootIdentity: identity,
		}},
	}

	changed := orch.receiveParentRunnerPublicationFromParent(parent, publication)
	require.True(t, changed)
	identity.Device = 99

	cached := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, cached.Children, 1)
	require.NotNil(t, cached.Children[0].LocalRootIdentity)
	assert.Equal(t, uint64(1), cached.Children[0].LocalRootIdentity.Device)

	cached.Children[0].LocalRootIdentity.Device = 42
	cachedAgain := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.NotNil(t, cachedAgain.Children[0].LocalRootIdentity)
	assert.Equal(t, uint64(1), cachedAgain.Children[0].LocalRootIdentity.Device)
}

// Validates: R-2.4.8, R-4.1.4
func TestReceiveParentRunnerPublication_UsesParentCleanupRequests(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
	})

	changed := orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		CleanupRequests: []syncengine.ShortcutChildArtifactCleanupRequest{{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcuts/Old",
			Reason:            syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	})

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.CleanupRequests, 1)
	assert.Equal(t, "binding-old", publication.CleanupRequests[0].BindingItemID)

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent}),
		nil,
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.CleanupChildren, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), compiled.CleanupChildren[0].mountID)
	assert.Equal(t, filepath.Join(parent.syncRoot, "Shortcuts", "Old"), compiled.CleanupChildren[0].localRoot)

	changed = orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		CleanupRequests: []syncengine.ShortcutChildArtifactCleanupRequest{{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcuts/Old",
			Reason:            syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	})
	assert.False(t, changed)
	publication = orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.CleanupRequests, 1)
	assert.Equal(t, "binding-old", publication.CleanupRequests[0].BindingItemID)
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestParentWaitingReplacementPublishesOnlyOldFinalDrainChild(t *testing.T) {
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

	changed := orch.receiveParentRunnerPublicationFromParent(parent, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children:    []syncengine.ShortcutChildRunner{topologyForTest(&root)},
	})

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.Children, 1)
	assert.Equal(t, "binding-old", publication.Children[0].BindingItemID)
	assert.Equal(t, syncengine.ShortcutChildActionFinalDrain, publication.Children[0].RunnerAction)
}

// Validates: R-2.8.1, R-4.1.4
func TestCompileRuntimeMountsFromParentRunnerPublication_DoesNotClassifyDuplicateAutomaticChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := testParentRoot(parent, "binding-a", "Shortcuts/A")
	second := testParentRoot(parent, "binding-b", "Shortcuts/B")
	orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{
			topologyForTest(&first),
			topologyForTest(&second),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent}),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, compiled.Mounts, 3)
	assert.Empty(t, compiled.Skipped)
}

// Validates: R-2.8.1, R-4.1.4
func TestCompileRuntimeMountsFromParentRunnerPublication_StandaloneContentRootRunsBesideChild(t *testing.T) {
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
	orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
		Children: []syncengine.ShortcutChildRunner{
			topologyForTest(&root),
		},
	})

	compiled, err := compileRuntimeMountsForParents(
		[]*mountSpec{parent, standalone},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent, standalone}),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, compiled.Mounts, 3)
	assert.Empty(t, compiled.Skipped)
}
