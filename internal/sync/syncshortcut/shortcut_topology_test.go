package syncshortcut

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.4.3, R-2.4.8
func TestShortcutObservationBatch_ShouldApplyCompleteEvenWithoutFacts(t *testing.T) {
	t.Parallel()

	assert.True(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationComplete,
	}.ShouldApply())
	assert.True(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationIncremental,
		Deletes: []ShortcutBindingDelete{{
			BindingItemID: "binding-1",
		}},
	}.ShouldApply())
	assert.False(t, ShortcutTopologyBatch{
		Kind: ShortcutTopologyObservationIncremental,
	}.ShouldApply())
}

// Validates: R-2.4.8
func TestShortcutChildWorkSnapshotIncludesExplicitCleanupScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		syncfixture.ShortcutNamespaceTestID,
		parentRoot,
		syncdomain.ContentFilterConfig{},
		[]ShortcutRootRecord{{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-cleanup",
			RelativeLocalPath: "Shortcuts/Old",
			State:             ShortcutRootStateRemovedChildCleanupPending,
		}},
	)

	require.Empty(t, publication.RunCommands)
	require.Len(t, publication.CleanupCommands, 1)
	cleanup := publication.CleanupCommands[0]
	assert.Equal(t, "personal:owner@example.com|binding:binding-cleanup", cleanup.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Old"), cleanup.LocalRoot)
	assert.False(t, cleanup.AckRef.IsZero())
}

// Validates: R-2.4.8
func TestShortcutChildWorkSnapshotIncludesExplicitRunnerScope(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		syncfixture.ShortcutNamespaceTestID,
		parentRoot,
		syncdomain.ContentFilterConfig{},
		[]ShortcutRootRecord{{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-run",
			RelativeLocalPath: "Shortcuts/Run",
			LocalAlias:        "Run",
			RemoteDriveID:     driveid.New("remote-drive"),
			RemoteItemID:      "remote-root",
			State:             ShortcutRootStateActive,
		}},
	)

	require.Len(t, publication.RunCommands, 1)
	child := publication.RunCommands[0]
	assert.Equal(t, "personal:owner@example.com|binding:binding-run", child.ChildMountID)
	assert.Equal(t, filepath.Join(parentRoot, "Shortcuts", "Run"), child.Engine.LocalRoot)
	assert.Equal(t, ShortcutChildRunModeNormal, child.Mode)
	assert.False(t, child.AckRef.IsZero())
}

// Validates: R-2.4.8
func TestShortcutChildWorkSnapshotProjectsParentContentFilter(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		syncfixture.ShortcutNamespaceTestID,
		parentRoot,
		syncdomain.ContentFilterConfig{
			IgnoredDirs:     []string{"Shortcuts/Run/cache"},
			IncludedDirs:    []string{"Shortcuts/Run/Docs"},
			IgnoredPaths:    []string{"*.tmp", "Shortcuts/*/Logs", "Other/*.bak"},
			IgnoreDotfiles:  true,
			IgnoreJunkFiles: true,
			FollowSymlinks:  true,
		},
		[]ShortcutRootRecord{{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-run",
			RelativeLocalPath: "Shortcuts/Run",
			LocalAlias:        "Run",
			RemoteDriveID:     driveid.New("remote-drive"),
			RemoteItemID:      "remote-root",
			State:             ShortcutRootStateActive,
		}},
	)

	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, syncdomain.ContentFilterConfig{
		IgnoredDirs:     []string{"cache"},
		IncludedDirs:    []string{"Docs"},
		IgnoredPaths:    []string{"*.tmp", "Logs"},
		IgnoreDotfiles:  true,
		IgnoreJunkFiles: true,
		FollowSymlinks:  true,
	}, publication.RunCommands[0].Engine.ContentFilter)
}

// Validates: R-2.4.8
func TestShortcutChildWorkSnapshotSkipsHiddenAlias(t *testing.T) {
	t.Parallel()

	parentRoot := filepath.Join(t.TempDir(), "parent")
	publication := ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		syncfixture.ShortcutNamespaceTestID,
		parentRoot,
		syncdomain.ContentFilterConfig{IgnoredDirs: []string{"Shortcuts/Run"}},
		[]ShortcutRootRecord{{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-run",
			RelativeLocalPath: "Shortcuts/Run",
			LocalAlias:        "Run",
			RemoteDriveID:     driveid.New("remote-drive"),
			RemoteItemID:      "remote-root",
			State:             ShortcutRootStateActive,
		}},
	)

	assert.Empty(t, publication.RunCommands)
}

// Validates: R-2.4.8
func TestShortcutChildWorkSnapshotEqualityIsSyncOwned(t *testing.T) {
	t.Parallel()

	identity := &ShortcutRootIdentity{Device: 1, Inode: 2}
	first := NormalizeShortcutChildWorkSnapshot(syncfixture.ShortcutNamespaceTestID, ShortcutChildWorkSnapshot{
		RunCommands: []ShortcutChildRunCommand{{
			ChildMountID: "personal:owner@example.com|binding:binding-b",
			DisplayName:  "B",
			Engine: ShortcutChildEngineSpec{
				LocalRootIdentity: identity,
			},
			Mode:   ShortcutChildRunModeNormal,
			AckRef: NewShortcutChildAckRefForTest("binding-b"),
		}, {
			ChildMountID: "personal:owner@example.com|binding:binding-a",
			DisplayName:  "A",
			Mode:         ShortcutChildRunModeFinalDrain,
			AckRef:       NewShortcutChildAckRefForTest("binding-a"),
		}},
		CleanupCommands: []ShortcutChildCleanupCommand{{
			ChildMountID: "personal:owner@example.com|binding:cleanup-b",
			LocalRoot:    filepath.Join("parent", "B"),
			Reason:       ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       NewShortcutChildAckRefForTest("cleanup-b"),
		}},
	})
	identity.Device = 99
	second := NormalizeShortcutChildWorkSnapshot(syncfixture.ShortcutNamespaceTestID, ShortcutChildWorkSnapshot{
		NamespaceID: syncfixture.ShortcutNamespaceTestID,
		RunCommands: []ShortcutChildRunCommand{{
			ChildMountID: "personal:owner@example.com|binding:binding-a",
			DisplayName:  "A",
			Mode:         ShortcutChildRunModeFinalDrain,
			AckRef:       NewShortcutChildAckRefForTest("binding-a"),
		}, {
			ChildMountID: "personal:owner@example.com|binding:binding-b",
			DisplayName:  "B",
			Engine: ShortcutChildEngineSpec{
				LocalRootIdentity: &ShortcutRootIdentity{Device: 1, Inode: 2},
			},
			Mode:   ShortcutChildRunModeNormal,
			AckRef: NewShortcutChildAckRefForTest("binding-b"),
		}},
		CleanupCommands: []ShortcutChildCleanupCommand{{
			ChildMountID: "personal:owner@example.com|binding:cleanup-b",
			LocalRoot:    filepath.Join("parent", "B"),
			Reason:       ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       NewShortcutChildAckRefForTest("cleanup-b"),
		}},
	})

	assert.True(t, ShortcutChildWorkSnapshotsEqual(first, second))
	assert.Equal(t, uint64(1), first.RunCommands[1].Engine.LocalRootIdentity.Device)
}

// Validates: R-2.4.8
func TestShortcutChildAckHandleZeroReturnsExplicitErrors(t *testing.T) {
	t.Parallel()

	handle := ShortcutChildAckHandle{}

	_, err := handle.AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		Ref: NewShortcutChildAckRefForTest("binding-1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child final-drain ack requires live parent")

	_, err = handle.AcknowledgeChildArtifactsPurged(t.Context(), ShortcutChildArtifactCleanupAck{
		Ref: NewShortcutChildAckRefForTest("binding-1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shortcut child artifact cleanup ack requires live parent")
}
