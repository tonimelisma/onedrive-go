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

func testPublishedShortcutChild() syncengine.ShortcutChildRunner {
	const (
		relativePath  = "Shortcuts/Docs"
		remoteDriveID = "remote-drive"
		remoteItemID  = "remote-root"
	)
	return syncengine.ShortcutChildRunner{
		BindingItemID:     "binding-child-docs",
		LocalAlias:        filepath.Base(relativePath),
		RelativeLocalPath: relativePath,
		RemoteDriveID:     remoteDriveID,
		RemoteItemID:      remoteItemID,
		RunnerAction:      syncengine.ShortcutChildActionRun,
	}
}

func testParentTopologies(
	parent *StandaloneMountConfig,
	children ...syncengine.ShortcutChildRunner,
) map[mountID]syncengine.ShortcutChildRunnerPublication {
	if parent == nil {
		return nil
	}
	return map[mountID]syncengine.ShortcutChildRunnerPublication{
		mountID(parent.CanonicalID.String()): {
			NamespaceID: parent.CanonicalID.String(),
			Children:    children,
		},
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
	assert.Equal(t, first.CanonicalID, mounts[0].canonicalID)
	assert.Equal(t, first.CanonicalID.DriveType(), mounts[0].driveType)
	assert.False(t, mounts[0].rejectSharePointRootForms)
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

	cfg, err := engineMountConfigForMount(mounts[0])
	require.NoError(t, err)

	assert.Equal(t, mounts[0].statePath, cfg.DBPath)
	assert.Equal(t, mounts[0].syncRoot, cfg.SyncRoot)
	assert.Equal(t, config.DefaultDataDir(), cfg.DataDir)
	assert.Equal(t, mounts[0].remoteDriveID, cfg.DriveID)
	assert.Equal(t, mounts[0].driveType, cfg.DriveType)
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
func TestCompileRuntimeMounts_AddsChildProjectionAfterParent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.TransferWorkers = 7
	parent.CheckWorkers = 8
	parent.MinFreeSpaceBytes = 5 * 1024 * 1024

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, testPublishedShortcutChild()),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.Empty(t, compiled.Skipped)

	parentMount := compiled.Mounts[0]
	childMount := compiled.Mounts[1]

	assert.Equal(t, MountProjectionStandalone, parentMount.projectionKind)
	assert.Equal(t, MountProjectionChild, childMount.projectionKind)
	assert.Equal(t, mountID(config.ChildMountID(parent.CanonicalID.String(), "binding-child-docs")), childMount.mountID)
	assert.Equal(t, parentMount.mountID, childMount.parentMountID)
	assert.True(t, childMount.canonicalID.IsZero())
	assert.Empty(t, childMount.driveType)
	assert.False(t, childMount.rejectSharePointRootForms)
	assert.Equal(t, filepath.Join(parent.SyncRoot, "Shortcuts", "Docs"), childMount.syncRoot)
	assert.Equal(t, config.MountStatePath(config.ChildMountID(parent.CanonicalID.String(), "binding-child-docs")), childMount.statePath)
	assert.Equal(t, driveid.MustCanonicalID("personal:owner@example.com"), childMount.tokenOwnerCanonical)
	assert.Equal(t, "owner@example.com", childMount.accountEmail)
	assert.Equal(t, 7, childMount.transferWorkers)
	assert.Equal(t, 8, childMount.checkWorkers)
	assert.Equal(t, int64(5*1024*1024), childMount.minFreeSpace)

	engineCfg, err := engineMountConfigForMount(childMount)
	require.NoError(t, err)
	assert.Equal(t, driveid.New("remote-drive"), engineCfg.DriveID)
	assert.Equal(t, "remote-root", engineCfg.RemoteRootItemID)
	assert.Empty(t, engineCfg.DriveType)
	assert.False(t, engineCfg.LocalRules.RejectSharePointRootForms)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_ParentPausePausesChildWithoutParentReservationSynthesis(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.Paused = true

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, testPublishedShortcutChild()),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.Empty(t, compiled.Skipped)

	childMount := compiled.Mounts[1]
	assert.True(t, childMount.paused)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_ParentBlockedChildDoesNotSynthesizeParentReservation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild()
	child.RunnerAction = syncengine.ShortcutChildActionSkipParentBlocked
	child.RunnerDetail = "duplicate_content_root"

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, child),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "duplicate_content_root")
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_FinalDrainChildDoesNotSynthesizeParentReservation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild()
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, child),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	require.Empty(t, compiled.Skipped)
	assert.Equal(t, []string{config.ChildMountID(parent.CanonicalID.String(), "binding-child-docs")}, compiled.FinalDrainMountIDs)
	assert.True(t, compiled.Mounts[1].finalDrain)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_UsesParentRunnerRelativePathOnly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild()
	child.RelativeLocalPath = "Shortcuts/New Docs"

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, child),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.Equal(t, filepath.Join(parent.SyncRoot, "Shortcuts", "New Docs"), compiled.Mounts[1].syncRoot)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_UnavailableChildDoesNotSynthesizeParentReservation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	child := testPublishedShortcutChild()
	child.RemoteDriveID = ""
	child.RemoteItemID = ""
	child.RunnerAction = syncengine.ShortcutChildActionSkipParentBlocked
	child.RunnerDetail = "shortcut_binding_unavailable"

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, child),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "shortcut_binding_unavailable")
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_ChildDeltaCapabilityComesFromMountTokenOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "business:owner@example.com", "Parent")
	parent.RemoteRootDeltaCapable = false
	child := testPublishedShortcutChild()
	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		testParentTopologies(&parent, child),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.False(t, compiled.Mounts[1].remoteRootDeltaCapable)
	assert.Equal(t, parent.TokenOwnerCanonical, compiled.Mounts[1].tokenOwnerCanonical)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_StandaloneContentRootDoesNotSuppressChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "business:owner@example.com", "Parent")
	standalone := testStandaloneMount(t, "sharepoint:owner@example.com:site:Docs", "Standalone")
	standalone.RemoteDriveID = driveid.New("remote-drive")
	standalone.RemoteRootItemID = "remote-root"

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent, standalone},
		testParentTopologies(&parent, testPublishedShortcutChild()),
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 3)
	assert.Empty(t, compiled.Skipped)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_MissingParentSkipsChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")

	compiled, err := compileRuntimeMounts(
		[]StandaloneMountConfig{parent},
		map[mountID]syncengine.ShortcutChildRunnerPublication{
			"missing-parent": {
				NamespaceID: "missing-parent",
				Children: []syncengine.ShortcutChildRunner{{
					BindingItemID:     "binding-child-docs",
					RelativeLocalPath: "Shortcuts/Docs",
					RemoteDriveID:     "remote-drive",
					RemoteItemID:      "remote-root",
					RunnerAction:      syncengine.ShortcutChildActionRun,
				}},
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "missing parent mount")
}
