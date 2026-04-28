package multisync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func testParentProcessRoot() string {
	return filepath.Join(string(filepath.Separator), "tmp", "parent")
}

func testParentMountSpec() *mountSpec {
	return &mountSpec{
		mountID:             mountID("personal:owner@example.com"),
		projectionKind:      MountProjectionStandalone,
		tokenOwnerCanonical: driveid.MustCanonicalID("personal:owner@example.com"),
		remoteDriveID:       driveid.New("parent-drive"),
		syncRoot:            "/tmp/parent",
		parent: &parentMountRuntime{
			canonicalID: driveid.MustCanonicalID("personal:owner@example.com"),
			driveType:   "personal",
		},
	}
}

func testChildRecord(t *testing.T, parentID mountID, bindingID, relativePath string) syncengine.ShortcutChildRunCommand {
	t.Helper()

	return testChildRecordWithContext(t.Context(), t, parentID, bindingID, relativePath)
}

func testChildRecordWithContext(
	ctx context.Context,
	t *testing.T,
	parentID mountID,
	bindingID string,
	relativePath string,
) syncengine.ShortcutChildRunCommand {
	t.Helper()

	return syncengine.ShortcutChildRunCommand{
		ChildMountID: config.ChildMountID(parentID.String(), bindingID),
		DisplayName:  filepath.Base(filepath.FromSlash(relativePath)),
		Engine: syncengine.ShortcutChildEngineSpec{
			LocalRoot:     filepath.Join(testParentProcessRoot(), filepath.FromSlash(relativePath)),
			RemoteDriveID: "remote-drive",
			RemoteItemID:  "remote-root",
		},
		Mode:   syncengine.ShortcutChildRunModeNormal,
		AckRef: testShortcutChildAckRefWithContext(ctx, t, bindingID),
	}
}

func finalDrainChildRecord(t *testing.T, parentID mountID, bindingID, relativePath string) syncengine.ShortcutChildRunCommand {
	t.Helper()

	child := testChildRecord(t, parentID, bindingID, relativePath)
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	return child
}

func processSnapshot(namespaceID string, children ...syncengine.ShortcutChildRunCommand) syncengine.ShortcutChildWorkSnapshot {
	return processSnapshotForRoot(namespaceID, "/tmp/parent", children...)
}

func processSnapshotForParent(
	parent *StandaloneMountConfig,
	children ...syncengine.ShortcutChildRunCommand,
) syncengine.ShortcutChildWorkSnapshot {
	namespaceID := ""
	parentRoot := ""
	if parent != nil {
		namespaceID = parent.CanonicalID.String()
		parentRoot = parent.SyncRoot
	}
	return processSnapshotForRoot(namespaceID, parentRoot, children...)
}

func processSnapshotForRoot(
	namespaceID string,
	parentRoot string,
	children ...syncengine.ShortcutChildRunCommand,
) syncengine.ShortcutChildWorkSnapshot {
	for i := range children {
		if children[i].Engine.LocalRoot != "" && parentRoot != "" {
			rel, relErr := filepath.Rel(testParentProcessRoot(), children[i].Engine.LocalRoot)
			if relErr == nil && rel != "." && !filepath.IsAbs(rel) &&
				rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				children[i].Engine.LocalRoot = filepath.Join(parentRoot, rel)
			}
		}
	}
	return syncengine.ShortcutChildWorkSnapshot{
		NamespaceID: namespaceID,
		RunCommands: children,
	}
}

func cleanupRequestSnapshot(
	namespaceID string,
	requests ...syncengine.ShortcutChildCleanupCommand,
) syncengine.ShortcutChildWorkSnapshot {
	return syncengine.ShortcutChildWorkSnapshot{
		NamespaceID:     namespaceID,
		CleanupCommands: requests,
	}
}

func seedShortcutChildRunCommand(
	orch *Orchestrator,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildRunCommand,
) {
	if parent == nil || child == nil || child.ChildMountID == "" {
		return
	}
	orch.receiveParentChildWorkSnapshot(
		mountID(parent.CanonicalID.String()),
		processSnapshotForParent(parent, *child),
	)
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentChildWorkSnapshot_StoresSnapshotInMemory(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})

	changed := orch.receiveParentChildWorkSnapshotFromParent(parent, processSnapshot(
		parent.mountID.String(),
		testChildRecord(t, parent.mountID, "binding-1", "Shared/Docs"),
	))

	assert.True(t, changed)
	publication := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, syncengine.ShortcutChildRunModeNormal, publication.RunCommands[0].Mode)
	assert.Equal(t, filepath.Join(parent.syncRoot, "Shared", "Docs"), publication.RunCommands[0].Engine.LocalRoot)
	assert.Equal(t, "remote-drive", publication.RunCommands[0].Engine.RemoteDriveID)
	assert.Equal(t, "remote-root", publication.RunCommands[0].Engine.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestReceiveParentChildWorkSnapshot_EquivalentSnapshotIsStable(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := processSnapshot(
		parent.mountID.String(),
		testChildRecord(t, parent.mountID, "binding-1", "Shared/Docs"),
	)
	equivalent := processSnapshot(
		parent.mountID.String(),
		testChildRecord(t, parent.mountID, "binding-1", "Shared/Docs"),
	)

	assert.True(t, orch.receiveParentChildWorkSnapshotFromParent(parent, first))
	assert.False(t, orch.receiveParentChildWorkSnapshotFromParent(parent, equivalent))
}

// Validates: R-2.4.8, R-2.8.1, R-4.1.4
func TestReceiveParentChildWorkSnapshot_EmptySnapshotClearsCachedChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentChildWorkSnapshot(parent.mountID, processSnapshot(
		parent.mountID.String(),
		testChildRecord(t, parent.mountID, "binding-old", "Shortcut"),
	))

	changed := orch.receiveParentChildWorkSnapshotFromParent(parent, syncengine.ShortcutChildWorkSnapshot{
		NamespaceID: parent.mountID.String(),
	})

	assert.True(t, changed)
	publication := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	assert.Empty(t, publication.RunCommands)
}

// Validates: R-2.8.1, R-4.1.4
func TestParentChildWorkSnapshotCache_ClonesSnapshot(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	identity := &syncengine.ShortcutRootIdentity{Device: 1, Inode: 2}
	child := testChildRecord(t, parent.mountID, "binding-1", "Shortcuts/Docs")
	child.Engine.LocalRootIdentity = identity
	publication := processSnapshot(parent.mountID.String(), child)

	changed := orch.receiveParentChildWorkSnapshotFromParent(parent, publication)
	require.True(t, changed)
	identity.Device = 99

	cached := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.Len(t, cached.RunCommands, 1)
	require.NotNil(t, cached.RunCommands[0].Engine.LocalRootIdentity)
	assert.Equal(t, uint64(1), cached.RunCommands[0].Engine.LocalRootIdentity.Device)

	cached.RunCommands[0].Engine.LocalRootIdentity.Device = 42
	cachedAgain := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.NotNil(t, cachedAgain.RunCommands[0].Engine.LocalRootIdentity)
	assert.Equal(t, uint64(1), cachedAgain.RunCommands[0].Engine.LocalRootIdentity.Device)
}

// Validates: R-2.4.8, R-4.1.4
func TestReceiveParentChildWorkSnapshot_UsesParentCleanupRequests(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	orch.receiveParentChildWorkSnapshot(parent.mountID, syncengine.ShortcutChildWorkSnapshot{
		NamespaceID: parent.mountID.String(),
	})

	changed := orch.receiveParentChildWorkSnapshot(parent.mountID, cleanupRequestSnapshot(
		parent.mountID.String(),
		syncengine.ShortcutChildCleanupCommand{
			ChildMountID: config.ChildMountID(parent.mountID.String(), "binding-old"),
			LocalRoot:    filepath.Join(parent.syncRoot, "Shortcuts", "Old"),
			Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       testShortcutChildAckRef(t, "binding-old"),
		},
	))

	assert.True(t, changed)
	publication := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.Len(t, publication.CleanupCommands, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), publication.CleanupCommands[0].ChildMountID)

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		orch.latestParentChildWorkSnapshotsFor([]*mountSpec{parent}),
		t.TempDir(),
		nil,
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 1)
	require.Len(t, decisions.CleanupChildren, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), decisions.CleanupChildren[0].mountID)
	assert.Equal(t, filepath.Join(parent.syncRoot, "Shortcuts", "Old"), decisions.CleanupChildren[0].localRoot)

	changed = orch.receiveParentChildWorkSnapshot(parent.mountID, cleanupRequestSnapshot(
		parent.mountID.String(),
		syncengine.ShortcutChildCleanupCommand{
			ChildMountID: config.ChildMountID(parent.mountID.String(), "binding-old"),
			LocalRoot:    filepath.Join(parent.syncRoot, "Shortcuts", "Old"),
			Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       testShortcutChildAckRef(t, "binding-old"),
		},
	))
	assert.False(t, changed)
	publication = orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.Len(t, publication.CleanupCommands, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), publication.CleanupCommands[0].ChildMountID)
}

// Validates: R-2.4.8, R-4.1.4
func TestParentCleanupRequestUsesExplicitArtifactScope(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	requestedRoot := filepath.Join(t.TempDir(), "published-child-root")
	requestedMountID := config.ChildMountID(parent.mountID.String(), "binding-cleanup")
	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		map[mountID]syncengine.ShortcutChildWorkSnapshot{
			parent.mountID: cleanupRequestSnapshot(
				parent.mountID.String(),
				syncengine.ShortcutChildCleanupCommand{
					ChildMountID: requestedMountID,
					LocalRoot:    requestedRoot,
					Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
					AckRef:       testShortcutChildAckRef(t, "binding-cleanup"),
				},
			),
		},
		t.TempDir(),
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

	parent := testParentMountSpec()
	cases := []struct {
		name    string
		request syncengine.ShortcutChildCleanupCommand
		want    string
	}{
		{
			name: "child mount ID",
			request: syncengine.ShortcutChildCleanupCommand{
				LocalRoot: filepath.Join(t.TempDir(), "child"),
				Reason:    syncengine.ShortcutChildArtifactCleanupParentRemoved,
				AckRef:    testShortcutChildAckRef(t, "binding-cleanup"),
			},
			want: "missing child mount ID",
		},
		{
			name: "local root",
			request: syncengine.ShortcutChildCleanupCommand{
				ChildMountID: config.ChildMountID(parent.mountID.String(), "binding-cleanup"),
				Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
				AckRef:       testShortcutChildAckRef(t, "binding-cleanup"),
			},
			want: "missing child local root",
		},
		{
			name: "reason",
			request: syncengine.ShortcutChildCleanupCommand{
				ChildMountID: config.ChildMountID(parent.mountID.String(), "binding-cleanup"),
				LocalRoot:    filepath.Join(t.TempDir(), "child"),
				AckRef:       testShortcutChildAckRef(t, "binding-cleanup"),
			},
			want: "missing cleanup reason",
		},
		{
			name: "ack ref",
			request: syncengine.ShortcutChildCleanupCommand{
				ChildMountID: config.ChildMountID(parent.mountID.String(), "binding-cleanup"),
				LocalRoot:    filepath.Join(t.TempDir(), "child"),
				Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
			},
			want: "missing acknowledgement reference",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildRunnerDecisionsForParents(
				[]*mountSpec{parent},
				map[mountID]syncengine.ShortcutChildWorkSnapshot{
					parent.mountID: cleanupRequestSnapshot(parent.mountID.String(), tt.request),
				},
				t.TempDir(),
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
	changed := orch.receiveParentChildWorkSnapshotFromParent(parent, processSnapshot(
		parent.mountID.String(),
		finalDrainChildRecord(t, parent.mountID, "binding-old", "Shortcut"),
	))

	assert.True(t, changed)
	publication := orch.latestParentChildWorkSnapshotFor(parent.mountID)
	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, config.ChildMountID(parent.mountID.String(), "binding-old"), publication.RunCommands[0].ChildMountID)
	assert.Equal(t, syncengine.ShortcutChildRunModeFinalDrain, publication.RunCommands[0].Mode)
}

// Validates: R-2.8.1, R-4.1.4
func TestBuildRunnerDecisionsFromParentChildWorkSnapshot_DoesNotClassifyDuplicateAutomaticChildren(t *testing.T) {
	t.Parallel()

	parent := testParentMountSpec()
	orch := NewOrchestrator(&OrchestratorConfig{})
	first := testChildRecord(t, parent.mountID, "binding-a", "Shortcuts/A")
	second := testChildRecord(t, parent.mountID, "binding-b", "Shortcuts/B")
	orch.receiveParentChildWorkSnapshot(parent.mountID, processSnapshot(parent.mountID.String(), first, second))

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent},
		orch.latestParentChildWorkSnapshotsFor([]*mountSpec{parent}),
		t.TempDir(),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, decisions.Mounts, 3)
	assert.Empty(t, decisions.Skipped)
}

// Validates: R-2.8.1, R-4.1.4
func TestBuildRunnerDecisionsFromParentChildWorkSnapshot_StandaloneContentRootRunsBesideChild(t *testing.T) {
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
	child := testChildRecord(t, parent.mountID, "binding-a", "Shortcuts/A")
	orch.receiveParentChildWorkSnapshot(parent.mountID, processSnapshot(parent.mountID.String(), child))

	decisions, err := buildRunnerDecisionsForParents(
		[]*mountSpec{parent, standalone},
		orch.latestParentChildWorkSnapshotsFor([]*mountSpec{parent, standalone}),
		t.TempDir(),
		nil,
	)
	require.NoError(t, err)
	assert.Len(t, decisions.Mounts, 3)
	assert.Empty(t, decisions.Skipped)
}
