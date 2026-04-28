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

const testMountRemoteRootItemID = "mount-root-id"

func testPublishedShortcutChild(t *testing.T) syncengine.ShortcutChildRunCommand {
	t.Helper()

	const (
		relativePath  = "Shortcuts/Docs"
		remoteDriveID = "remote-drive"
		remoteItemID  = "remote-root"
	)
	return syncengine.ShortcutChildRunCommand{
		ChildMountID: config.ChildMountID("personal:owner@example.com", "binding-child-docs"),
		DisplayName:  filepath.Base(relativePath),
		Engine: syncengine.ShortcutChildEngineSpec{
			LocalRoot:     filepath.Join(testParentProcessRoot(), filepath.FromSlash(relativePath)),
			RemoteDriveID: remoteDriveID,
			RemoteItemID:  remoteItemID,
		},
		Mode:   syncengine.ShortcutChildRunModeNormal,
		AckRef: testShortcutChildAckRef(t, "binding-child-docs"),
	}
}

func testParentProcessSnapshots(
	parent *StandaloneMountConfig,
	children ...syncengine.ShortcutChildRunCommand,
) map[mountID]syncengine.ShortcutChildWorkSnapshot {
	if parent == nil {
		return nil
	}
	return map[mountID]syncengine.ShortcutChildWorkSnapshot{
		mountID(parent.CanonicalID.String()): processSnapshotForParent(parent, children...),
	}
}

// Validates: R-2.8.1
func TestBuildStandaloneMountSpecs_PreservesOrderAndReportingFields(t *testing.T) {
	t.Parallel()

	first := testStandaloneMount(t, "personal:first@example.com", "First")
	second := testStandaloneMount(t, "business:second@example.com", "Second")
	second.Paused = true
	first.SelectionIndex = 4
	second.SelectionIndex = 9

	mounts, err := buildStandaloneMountSpecs([]StandaloneMountConfig{first, second})
	require.NoError(t, err)
	require.Len(t, mounts, 2)

	assert.Equal(t, mountID(first.CanonicalID.String()), mounts[0].mountID)
	assert.Equal(t, 4, mounts[0].selectionIndex)
	assert.Equal(t, MountProjectionStandalone, mounts[0].projectionKind)
	assert.Equal(t, first.CanonicalID, mounts[0].parentCanonicalID())
	assert.Equal(t, first.CanonicalID.DriveType(), mounts[0].parentDriveType())
	assert.False(t, mounts[0].rejectSharePointRootForms())
	assert.Equal(t, first.DisplayName, mounts[0].displayName)
	assert.Equal(t, first.SyncRoot, mounts[0].syncRoot)
	assert.Equal(t, first.StatePath, mounts[0].statePath)
	assert.Equal(t, first.RemoteDriveID, mounts[0].remoteDriveID)
	assert.Equal(t, first.TokenOwnerCanonical, mounts[0].tokenOwnerCanonical)
	assert.Equal(t, first.AccountEmail, mounts[0].accountEmail)
	assert.False(t, mounts[0].paused)
	assert.Equal(t, first.TransferWorkers, mounts[0].transferWorkers)
	assert.Equal(t, first.CheckWorkers, mounts[0].checkWorkers)

	assert.Equal(t, mountID(second.CanonicalID.String()), mounts[1].mountID)
	assert.Equal(t, 9, mounts[1].selectionIndex)
	assert.True(t, mounts[1].paused)
}

// Validates: R-2.8.1
func TestBuildStandaloneMountSpecs_PreservesRootedMountFields(t *testing.T) {
	t.Parallel()

	shared := testStandaloneMount(t, "business:owner@example.com", "Shared")
	shared.RemoteRootItemID = testMountRemoteRootItemID
	shared.RemoteRootDeltaCapable = true
	shared.EnableWebsocket = true
	shared.RemoteDriveID = driveid.New("remote-drive-id")
	shared.SelectionIndex = 1

	mounts, err := buildStandaloneMountSpecs([]StandaloneMountConfig{shared})
	require.NoError(t, err)
	require.Len(t, mounts, 1)

	assert.Equal(t, testMountRemoteRootItemID, mounts[0].remoteRootItemID)
	assert.Equal(t, driveid.New("remote-drive-id"), mounts[0].remoteDriveID)
	assert.True(t, mounts[0].remoteRootDeltaCapable)
	assert.True(t, mounts[0].enableWebsocket)
}

// Validates: R-2.8.1
func TestParentMountSpecLoweringDoesNotCarryChildRuntimeState(t *testing.T) {
	t.Parallel()

	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")

	spec, err := newParentMountSpec(&parent)
	require.NoError(t, err)

	mount := spec.runtimeMountSpec()
	assert.Equal(t, MountProjectionStandalone, mount.projectionKind)
	assert.Nil(t, mount.child)
	assert.Empty(t, mount.childParentMountID())
	assert.False(t, mount.isFinalDrainChild())
}

// Validates: R-2.8.1
func TestBuildStandaloneMountSpecs_ZeroCanonicalIDFails(t *testing.T) {
	t.Parallel()

	_, err := buildStandaloneMountSpecs([]StandaloneMountConfig{{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "standalone mount canonical ID is required")
}

// Validates: R-2.8.1
func TestEngineMountConfigForMount_UsesMountOwnedFields(t *testing.T) {
	t.Parallel()

	shared := testStandaloneMount(t, "sharepoint:owner@example.com:site:Documents", "Shared")
	shared.RemoteRootItemID = testMountRemoteRootItemID
	shared.RemoteRootDeltaCapable = true
	shared.EnableWebsocket = true
	shared.RemoteDriveID = driveid.New("remote-drive-id")
	shared.TransferWorkers = 7
	shared.CheckWorkers = 8
	shared.MinFreeSpaceBytes = 3 * 1024 * 1024
	shared.SelectionIndex = 2

	mounts, err := buildStandaloneMountSpecs([]StandaloneMountConfig{shared})
	require.NoError(t, err)
	require.Len(t, mounts, 1)

	dataDir := t.TempDir()
	cfg, err := engineMountConfigForMount(mounts[0], dataDir)
	require.NoError(t, err)

	assert.Equal(t, mounts[0].statePath, cfg.DBPath)
	assert.Equal(t, mounts[0].syncRoot, cfg.SyncRoot)
	assert.Equal(t, dataDir, cfg.DataDir)
	assert.Equal(t, mounts[0].remoteDriveID, cfg.DriveID)
	assert.Equal(t, mounts[0].parentDriveType(), cfg.DriveType)
	assert.Equal(t, mounts[0].accountEmail, cfg.AccountEmail)
	assert.Equal(t, mounts[0].remoteRootItemID, cfg.RemoteRootItemID)
	assert.Equal(t, mounts[0].remoteRootDeltaCapable, cfg.RemoteRootDeltaCapable)
	assert.Equal(t, mounts[0].enableWebsocket, cfg.EnableWebsocket)
	assert.True(t, cfg.LocalRules.RejectSharePointRootForms)
	assert.Equal(t, 7, cfg.TransferWorkers)
	assert.Equal(t, 8, cfg.CheckWorkers)
	assert.Equal(t, int64(3*1024*1024), cfg.MinFreeSpace)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_AddsChildProjectionAfterParent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.TransferWorkers = 7
	parent.CheckWorkers = 8
	parent.MinFreeSpaceBytes = 5 * 1024 * 1024
	dataDir := t.TempDir()

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, testPublishedShortcutChild(t)),
		dataDir,
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	assert.Empty(t, decisions.Skipped)

	parentMount := decisions.Mounts[0]
	childMount := decisions.Mounts[1]

	assert.Equal(t, MountProjectionStandalone, parentMount.projectionKind)
	assert.Equal(t, MountProjectionChild, childMount.projectionKind)
	assert.Equal(t, mountID(config.ChildMountID(parent.CanonicalID.String(), "binding-child-docs")), childMount.mountID)
	assert.Equal(t, parentMount.mountID, childMount.childParentMountID())
	assert.True(t, childMount.parentCanonicalID().IsZero())
	assert.Empty(t, childMount.parentDriveType())
	assert.False(t, childMount.rejectSharePointRootForms())
	assert.Equal(t, filepath.Join(parent.SyncRoot, "Shortcuts", "Docs"), childMount.syncRoot)
	assert.Equal(t, config.MountStatePathForDataDir(dataDir, config.ChildMountID(parent.CanonicalID.String(), "binding-child-docs")), childMount.statePath)
	assert.Equal(t, driveid.MustCanonicalID("personal:owner@example.com"), childMount.tokenOwnerCanonical)
	assert.Equal(t, "owner@example.com", childMount.accountEmail)
	assert.Equal(t, 7, childMount.transferWorkers)
	assert.Equal(t, 8, childMount.checkWorkers)
	assert.Equal(t, int64(5*1024*1024), childMount.minFreeSpace)

	engineCfg, err := engineMountConfigForMount(childMount, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, driveid.New("remote-drive"), engineCfg.DriveID)
	assert.Equal(t, "remote-root", engineCfg.RemoteRootItemID)
	assert.Empty(t, engineCfg.DriveType)
	assert.False(t, engineCfg.LocalRules.RejectSharePointRootForms)
}

// Validates: R-2.8.1
func TestChildMountSpecLoweringCarriesChildRuntimeStateOnlyAtRunnerBoundary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parentCfg := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parentCfg.EnableWebsocket = true
	parentCfg.TransferWorkers = 3
	parentCfg.CheckWorkers = 4
	parentMount, err := buildStandaloneMountSpec(&parentCfg)
	require.NoError(t, err)

	child := testPublishedShortcutChild(t)
	child.ChildMountID = config.ChildMountID(parentCfg.CanonicalID.String(), "binding-child-docs")
	child.Engine.LocalRootIdentity = &syncengine.ShortcutRootIdentity{Device: 7, Inode: 8}
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain

	spec := newChildMountSpec(
		parentMount,
		&child,
		child.ChildMountID,
		child.DisplayName,
		config.MountStatePathForDataDir(t.TempDir(), child.ChildMountID),
		parentMount.tokenOwnerCanonical,
	)
	mount := spec.runtimeMountSpec()

	assert.Equal(t, MountProjectionChild, mount.projectionKind)
	assert.True(t, mount.parentCanonicalID().IsZero())
	assert.Equal(t, mountID(parentCfg.CanonicalID.String()), mount.childParentMountID())
	assert.True(t, mount.isFinalDrainChild())
	assert.Equal(t, child.Engine.LocalRoot, mount.syncRoot)
	assert.Equal(t, child.Engine.RemoteItemID, mount.remoteRootItemID)
	assert.Equal(t, parentMount.transferWorkers, mount.transferWorkers)
	assert.Equal(t, parentMount.checkWorkers, mount.checkWorkers)
	require.NotNil(t, mount.expectedChildRootIdentity())
	assert.Equal(t, uint64(7), mount.expectedChildRootIdentity().Device)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_ParentPausePausesChildWithoutParentReservationSynthesis(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.Paused = true

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, testPublishedShortcutChild(t)),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	assert.Empty(t, decisions.Skipped)

	childMount := decisions.Mounts[1]
	assert.True(t, childMount.paused)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_ParentBlockedSnapshotHasNoChildWork(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 1)
	assert.Empty(t, decisions.Skipped)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_FinalDrainChildDoesNotSynthesizeParentReservation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild(t)
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, child),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	require.Empty(t, decisions.Skipped)
	assert.True(t, decisions.Mounts[1].isFinalDrainChild())
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_UsesParentRunnerRelativePathOnly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild(t)
	child.Engine.LocalRoot = filepath.Join(parent.SyncRoot, "Shortcuts", "New Docs")

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, child),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	assert.Equal(t, filepath.Join(parent.SyncRoot, "Shortcuts", "New Docs"), decisions.Mounts[1].syncRoot)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_MalformedChildDoesNotSynthesizeParentReservation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild(t)
	child.Engine.RemoteDriveID = ""
	child.Engine.RemoteItemID = ""

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, child),
		t.TempDir(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing remote root")
	assert.Nil(t, decisions)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_ChildDeltaCapabilityComesFromMountTokenOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "business:owner@example.com", "Parent")
	parent.RemoteRootDeltaCapable = false
	child := testPublishedShortcutChild(t)
	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		testParentProcessSnapshots(&parent, child),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	assert.False(t, decisions.Mounts[1].remoteRootDeltaCapable)
	assert.Equal(t, parent.TokenOwnerCanonical, decisions.Mounts[1].tokenOwnerCanonical)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_StandaloneContentRootDoesNotSuppressChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "business:owner@example.com", "Parent")
	standalone := testStandaloneMount(t, "sharepoint:owner@example.com:site:Docs", "Standalone")
	standalone.RemoteDriveID = driveid.New("remote-drive")
	standalone.RemoteRootItemID = "remote-root"

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent, standalone},
		testParentProcessSnapshots(&parent, testPublishedShortcutChild(t)),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 3)
	assert.Empty(t, decisions.Skipped)
}

// Validates: R-2.8.1
func TestBuildRunnerDecisions_MissingParentSkipsChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")

	decisions, err := buildRunnerDecisions(
		[]StandaloneMountConfig{parent},
		map[mountID]syncengine.ShortcutChildWorkSnapshot{
			"missing-parent": processSnapshot(
				"missing-parent",
				testChildRecord(t, "missing-parent", "binding-child-docs", "Shortcuts/Docs"),
			),
		},
		t.TempDir(),
	)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 1)
	require.Len(t, decisions.Skipped, 1)
	assert.Contains(t, decisions.Skipped[0].Err.Error(), "missing parent mount")
}
