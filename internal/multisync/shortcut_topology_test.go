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

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_StoresPublicationInMemory(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})

	changed := orch.receiveParentRunnerPublicationFromParent(parent, runnerPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildRunner{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			DisplayName:       "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
		},
	))

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.RunnerWork.Children, 1)
	assert.Equal(t, syncengine.ShortcutChildActionRun, publication.RunnerWork.Children[0].RunnerAction)
	assert.Equal(t, "Shared/Docs", publication.RunnerWork.Children[0].RelativeLocalPath)
	assert.Equal(t, "remote-drive-0001", publication.RunnerWork.Children[0].RemoteDriveID)
	assert.Equal(t, "remote-root", publication.RunnerWork.Children[0].RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_EquivalentPublicationIsStable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := runnerPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildRunner{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			DisplayName:       "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
		},
	)
	equivalent := runnerPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildRunner{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			DisplayName:       "Docs",
			RemoteDriveID:     "remote-drive-0001",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
		},
	)

	assert.True(t, orch.receiveParentRunnerPublicationFromParent(parent, first))
	assert.False(t, orch.receiveParentRunnerPublicationFromParent(parent, equivalent))
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestReceiveParentRunnerPublication_EmptyPublicationClearsCachedChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentRunnerPublication(parent.mountID, runnerPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildRunner{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcut",
			DisplayName:       "Shortcut",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
		},
	))

	changed := orch.receiveParentRunnerPublicationFromParent(parent, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
	})

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	assert.Empty(t, publication.RunnerWork.Children)
}

// Validates: R-2.8.1, R-4.1.4
func TestParentRunnerPublicationCache_ClonesPublication(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	identity := &syncengine.ShortcutRootIdentity{Device: 1, Inode: 2}
	publication := runnerPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildRunner{
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shortcuts/Docs",
			DisplayName:       "Docs",
			RemoteDriveID:     "remote-drive",
			RemoteItemID:      "remote-root",
			RunnerAction:      syncengine.ShortcutChildActionRun,
			LocalRootIdentity: identity,
		},
	)

	changed := orch.receiveParentRunnerPublicationFromParent(parent, publication)
	require.True(t, changed)
	identity.Device = 99

	cached := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, cached.RunnerWork.Children, 1)
	require.NotNil(t, cached.RunnerWork.Children[0].LocalRootIdentity)
	assert.Equal(t, uint64(1), cached.RunnerWork.Children[0].LocalRootIdentity.Device)

	cached.RunnerWork.Children[0].LocalRootIdentity.Device = 42
	cachedAgain := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.NotNil(t, cachedAgain.RunnerWork.Children[0].LocalRootIdentity)
	assert.Equal(t, uint64(1), cachedAgain.RunnerWork.Children[0].LocalRootIdentity.Device)
}

// Validates: R-2.4.8, R-4.1.4
func TestReceiveParentRunnerPublication_UsesParentCleanupRequests(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentRunnerPublication(parent.mountID, syncengine.ShortcutChildRunnerPublication{
		NamespaceID: parent.mountID.String(),
	})

	changed := orch.receiveParentRunnerPublication(parent.mountID, cleanupRequestPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildArtifactCleanupRequest{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcuts/Old",
			ChildMountID:      config.ChildMountID(parent.mountID.String(), "binding-old"),
			LocalRoot:         filepath.Join(parent.syncRoot, "Shortcuts", "Old"),
			Reason:            syncengine.ShortcutChildArtifactCleanupParentRemoved,
		},
	))

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.CleanupWork.Requests, 1)
	assert.Equal(t, "binding-old", publication.CleanupWork.Requests[0].BindingItemID)

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent}),
		testRunnerDecisionDataDir(t),
		nil,
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 1)
	require.Len(t, decisions.CleanupChildren, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), decisions.CleanupChildren[0].mountID)
	assert.Equal(t, filepath.Join(parent.syncRoot, "Shortcuts", "Old"), decisions.CleanupChildren[0].localRoot)

	changed = orch.receiveParentRunnerPublication(parent.mountID, cleanupRequestPublication(
		parent.mountID.String(),
		syncengine.ShortcutChildArtifactCleanupRequest{
			BindingItemID:     "binding-old",
			RelativeLocalPath: "Shortcuts/Old",
			ChildMountID:      config.ChildMountID(parent.mountID.String(), "binding-old"),
			LocalRoot:         filepath.Join(parent.syncRoot, "Shortcuts", "Old"),
			Reason:            syncengine.ShortcutChildArtifactCleanupParentRemoved,
		},
	))
	assert.False(t, changed)
	publication = orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.CleanupWork.Requests, 1)
	assert.Equal(t, "binding-old", publication.CleanupWork.Requests[0].BindingItemID)
}

// Validates: R-2.4.8, R-4.1.4
func TestParentCleanupRequestUsesExplicitArtifactScope(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	requestedRoot := filepath.Join(t.TempDir(), "published-child-root")
	requestedMountID := config.ChildMountID(parent.mountID.String(), "binding-cleanup")
	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		map[mountID]syncengine.ShortcutChildRunnerPublication{
			parent.mountID: cleanupRequestPublication(
				parent.mountID.String(),
				syncengine.ShortcutChildArtifactCleanupRequest{
					BindingItemID:     "binding-cleanup",
					RelativeLocalPath: "Shortcuts/Old",
					ChildMountID:      requestedMountID,
					LocalRoot:         requestedRoot,
					Reason:            syncengine.ShortcutChildArtifactCleanupParentRemoved,
				},
			),
		},
		testRunnerDecisionDataDir(t),
		nil,
	)

	require.NoError(t, err)
	require.Len(t, decisions.CleanupChildren, 1)
	assert.Equal(t, requestedMountID, decisions.CleanupChildren[0].mountID)
	assert.Equal(t, requestedRoot, decisions.CleanupChildren[0].localRoot)
	assert.NotEqual(t, filepath.Join(parent.syncRoot, "Shortcuts", "Old"), decisions.CleanupChildren[0].localRoot)
}

// Validates: R-2.4.8, R-4.1.4
func TestParentCleanupRequestRequiresExplicitArtifactScope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		request syncengine.ShortcutChildArtifactCleanupRequest
		want    string
	}{
		{
			name: "child mount ID",
			request: syncengine.ShortcutChildArtifactCleanupRequest{
				BindingItemID: "binding-cleanup",
				LocalRoot:     filepath.Join(t.TempDir(), "child"),
				Reason:        syncengine.ShortcutChildArtifactCleanupParentRemoved,
			},
			want: "missing child mount ID",
		},
		{
			name: "local root",
			request: syncengine.ShortcutChildArtifactCleanupRequest{
				BindingItemID: "binding-cleanup",
				ChildMountID:  config.ChildMountID("personal:owner@example.com", "binding-cleanup"),
				Reason:        syncengine.ShortcutChildArtifactCleanupParentRemoved,
			},
			want: "missing child local root",
		},
		{
			name: "reason",
			request: syncengine.ShortcutChildArtifactCleanupRequest{
				BindingItemID: "binding-cleanup",
				ChildMountID:  config.ChildMountID("personal:owner@example.com", "binding-cleanup"),
				LocalRoot:     filepath.Join(t.TempDir(), "child"),
			},
			want: "missing cleanup reason",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent := testParentMountSpec()
			_, err := buildRunnerDecisionsForParents(
				[]*mountSpec{parent},
				map[mountID]syncengine.ShortcutChildRunnerPublication{
					parent.mountID: cleanupRequestPublication(parent.mountID.String(), tt.request),
				},
				testRunnerDecisionDataDir(t),
				nil,
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestParentSamePathReplacementPublishesOnlyOldFinalDrainChild(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	changed := orch.receiveParentRunnerPublicationFromParent(parent, runnerPublication(
		parent.mountID.String(),
		finalDrainChildRecord("binding-old", "Shortcut"),
	))

	assert.True(t, changed)
	publication := orch.latestParentRunnerPublicationFor(parent.mountID)
	require.Len(t, publication.RunnerWork.Children, 1)
	assert.Equal(t, "binding-old", publication.RunnerWork.Children[0].BindingItemID)
	assert.Equal(t, syncengine.ShortcutChildActionFinalDrain, publication.RunnerWork.Children[0].RunnerAction)
}

// Validates: R-2.8.1, R-4.1.4
func TestBuildRunnerDecisionsFromParentRunnerPublication_DoesNotClassifyDuplicateAutomaticChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	second := testChildRecord(parent.mountID, "binding-b", "Shortcuts/B")
	orch.receiveParentRunnerPublication(parent.mountID, runnerPublication(parent.mountID.String(), first, second))

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent}),
		testRunnerDecisionDataDir(t),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, decisions.Mounts, 3)
	assert.Empty(t, decisions.Skipped)
}

// Validates: R-2.8.1, R-4.1.4
func TestBuildRunnerDecisionsFromParentRunnerPublication_StandaloneContentRootRunsBesideChild(t *testing.T) {
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
	child := testChildRecord(parent.mountID, "binding-a", "Shortcuts/A")
	orch.receiveParentRunnerPublication(parent.mountID, runnerPublication(parent.mountID.String(), child))

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent, standalone},
		orch.latestParentRunnerPublicationsFor([]*mountSpec{parent, standalone}),
		testRunnerDecisionDataDir(t),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, decisions.Mounts, 3)
	assert.Empty(t, decisions.Skipped)
}
