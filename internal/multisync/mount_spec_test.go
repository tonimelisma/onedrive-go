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

const testRootedSubtreeItemID = "rooted-subtree-id"

// Validates: R-2.8.1
func TestBuildConfiguredMountSpecs_PreservesOrderAndReportingFields(t *testing.T) {
	t.Parallel()

	first := testResolvedDrive(t, "personal:first@example.com", "First")
	second := testResolvedDrive(t, "business:second@example.com", "Second")
	second.Paused = true

	mounts, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{
		{
			SelectionIndex: 4,
			Drive:          first,
		},
		{
			SelectionIndex: 9,
			Drive:          second,
		},
	})
	require.NoError(t, err)
	require.Len(t, mounts, 2)

	assert.Equal(t, mountID(first.CanonicalID.String()), mounts[0].mountID)
	assert.Equal(t, 4, mounts[0].selectionIndex)
	assert.Equal(t, mountProjectionStandalone, mounts[0].projectionKind)
	assert.Equal(t, first.CanonicalID, mounts[0].canonicalID)
	assert.Equal(t, first.DisplayName, mounts[0].displayName)
	assert.Equal(t, first.SyncDir, mounts[0].syncRoot)
	assert.Equal(t, first.StatePath(), mounts[0].statePath)
	assert.Equal(t, first.DriveID, mounts[0].remoteDriveID)
	assert.Equal(t, first.CanonicalID, mounts[0].tokenOwnerCanonical)
	assert.Equal(t, first.CanonicalID.Email(), mounts[0].accountEmail)
	assert.False(t, mounts[0].paused)
	assert.Equal(t, first.TransferWorkers, mounts[0].transferWorkers)
	assert.Equal(t, first.CheckWorkers, mounts[0].checkWorkers)

	assert.Equal(t, mountID(second.CanonicalID.String()), mounts[1].mountID)
	assert.Equal(t, 9, mounts[1].selectionIndex)
	assert.True(t, mounts[1].paused)
}

// Validates: R-2.8.1
func TestBuildConfiguredMountSpecs_PreservesRootedMountFields(t *testing.T) {
	t.Parallel()

	shared := testResolvedDrive(t, "business:owner@example.com", "Shared")
	shared.RootItemID = testRootedSubtreeItemID
	shared.SharedRootDeltaCapable = true
	shared.Websocket = true
	shared.DriveID = driveid.New("remote-drive-id")

	mounts, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{
		{
			SelectionIndex: 1,
			Drive:          shared,
		},
	})
	require.NoError(t, err)
	require.Len(t, mounts, 1)

	assert.Equal(t, testRootedSubtreeItemID, mounts[0].remoteRootItemID)
	assert.Equal(t, driveid.New("remote-drive-id"), mounts[0].remoteDriveID)
	assert.False(t, mounts[0].rootedSubtreeDeltaCapable)
	assert.True(t, mounts[0].enableWebsocket)
}

// Validates: R-2.8.1
func TestBuildConfiguredMountSpecs_NilDriveFails(t *testing.T) {
	t.Parallel()

	_, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{
		{
			SelectionIndex: 0,
			Drive:          nil,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved drive is required")
}

// Validates: R-2.8.1
func TestEngineMountConfigForMount_UsesMountOwnedFields(t *testing.T) {
	t.Parallel()

	shared := testResolvedDrive(t, "sharepoint:owner@example.com:site:Documents", "Shared")
	shared.RootItemID = testRootedSubtreeItemID
	shared.SharedRootDeltaCapable = true
	shared.Websocket = true
	shared.DriveID = driveid.New("remote-drive-id")
	shared.TransferWorkers = 7
	shared.CheckWorkers = 8
	shared.MinFreeSpace = "3MiB"

	mounts, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{{
		SelectionIndex: 2,
		Drive:          shared,
	}})
	require.NoError(t, err)
	require.Len(t, mounts, 1)

	cfg, err := engineMountConfigForMount(mounts[0])
	require.NoError(t, err)

	assert.Equal(t, mounts[0].statePath, cfg.DBPath)
	assert.Equal(t, mounts[0].syncRoot, cfg.SyncRoot)
	assert.Equal(t, config.DefaultDataDir(), cfg.DataDir)
	assert.Equal(t, mounts[0].remoteDriveID, cfg.DriveID)
	assert.Equal(t, mounts[0].canonicalID.DriveType(), cfg.DriveType)
	assert.Equal(t, mounts[0].accountEmail, cfg.AccountEmail)
	assert.Equal(t, mounts[0].remoteRootItemID, cfg.RootItemID)
	assert.Equal(t, mounts[0].rootedSubtreeDeltaCapable, cfg.RootedSubtreeDeltaCapable)
	assert.Equal(t, mounts[0].enableWebsocket, cfg.EnableWebsocket)
	assert.Equal(t, syncengine.LocalFilterConfig{}, cfg.LocalFilter)
	assert.True(t, cfg.LocalRules.RejectSharePointRootForms)
	assert.Equal(t, 7, cfg.TransferWorkers)
	assert.Equal(t, 8, cfg.CheckWorkers)
	assert.Equal(t, int64(3*1024*1024), cfg.MinFreeSpace)
}

// Validates: R-2.8.1
func TestEngineMountConfigForMount_InvalidMinFreeSpaceFails(t *testing.T) {
	t.Parallel()

	rd := testResolvedDrive(t, "personal:first@example.com", "First")
	rd.MinFreeSpace = "not-a-size"

	_, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{{
		SelectionIndex: 0,
		Drive:          rd,
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_AddsChildProjectionAfterParent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testResolvedDrive(t, "personal:owner@example.com", "Parent")
	parent.TransferWorkers = 7
	parent.CheckWorkers = 8
	parent.MinFreeSpace = "5MiB"

	compiled, err := compileRuntimeMounts(
		resolvedDrivesWithSelection([]*config.ResolvedDrive{parent}),
		&config.MountInventory{
			Mounts: map[string]config.MountRecord{
				"child-docs": {
					MountID:           "child-docs",
					ParentMountID:     parent.CanonicalID.String(),
					DisplayName:       "Docs",
					RelativeLocalPath: "Shortcuts/Docs",
					RemoteDriveID:     "remote-drive",
					RemoteRootItemID:  "remote-root",
				},
			},
		},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.Empty(t, compiled.Skipped)

	parentMount := compiled.Mounts[0]
	childMount := compiled.Mounts[1]

	assert.Equal(t, mountProjectionStandalone, parentMount.projectionKind)
	assert.Equal(t, []string{"Shortcuts/Docs"}, parentMount.localSkipDirs)
	assert.Equal(t, mountProjectionChild, childMount.projectionKind)
	assert.Equal(t, mountID("child-docs"), childMount.mountID)
	assert.Equal(t, driveid.MustCanonicalID("shared:owner@example.com:remote-drive:remote-root"), childMount.canonicalID)
	assert.Equal(t, filepath.Join(parent.SyncDir, "Shortcuts", "Docs"), childMount.syncRoot)
	assert.Equal(t, config.MountStatePath("child-docs"), childMount.statePath)
	assert.Equal(t, driveid.MustCanonicalID("personal:owner@example.com"), childMount.tokenOwnerCanonical)
	assert.Equal(t, "owner@example.com", childMount.accountEmail)
	assert.Equal(t, 7, childMount.transferWorkers)
	assert.Equal(t, 8, childMount.checkWorkers)
	assert.Equal(t, int64(5*1024*1024), childMount.minFreeSpace)
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_StandaloneContentRootSuppressesChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testResolvedDrive(t, "business:owner@example.com", "Parent")
	standalone := testResolvedDrive(t, "sharepoint:owner@example.com:site:Docs", "Standalone")
	standalone.DriveID = driveid.New("remote-drive")
	standalone.RootItemID = "remote-root"

	compiled, err := compileRuntimeMounts(
		resolvedDrivesWithSelection([]*config.ResolvedDrive{parent, standalone}),
		&config.MountInventory{
			Mounts: map[string]config.MountRecord{
				"child-docs": {
					MountID:           "child-docs",
					ParentMountID:     parent.CanonicalID.String(),
					RelativeLocalPath: "Shortcuts/Docs",
					RemoteDriveID:     "remote-drive",
					RemoteRootItemID:  "remote-root",
				},
			},
		},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	require.Len(t, compiled.Skipped, 1)
	assert.Empty(t, compiled.Mounts[0].localSkipDirs)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "standalone mount")
}

// Validates: R-2.8.1
func TestCompileRuntimeMounts_MissingParentSkipsChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testResolvedDrive(t, "personal:owner@example.com", "Parent")

	compiled, err := compileRuntimeMounts(
		resolvedDrivesWithSelection([]*config.ResolvedDrive{parent}),
		&config.MountInventory{
			Mounts: map[string]config.MountRecord{
				"child-docs": {
					MountID:           "child-docs",
					ParentMountID:     "missing-parent",
					RelativeLocalPath: "Shortcuts/Docs",
					RemoteDriveID:     "remote-drive",
					RemoteRootItemID:  "remote-root",
				},
			},
		},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), "missing parent mount")
}
