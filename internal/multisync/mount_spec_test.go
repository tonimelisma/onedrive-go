package multisync

import (
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
	assert.Equal(t, first.CanonicalID.Email(), mounts[0].accountEmail)
	assert.False(t, mounts[0].paused)
	assert.Same(t, first, mounts[0].resolved)

	assert.Equal(t, mountID(second.CanonicalID.String()), mounts[1].mountID)
	assert.Equal(t, 9, mounts[1].selectionIndex)
	assert.True(t, mounts[1].paused)
}

// Validates: R-2.8.1
func TestBuildConfiguredMountSpecs_PreservesRootedMountFields(t *testing.T) {
	t.Parallel()

	shared := testResolvedDrive(t, "shared:owner@example.com:test-drive:test-item", "Shared")
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
	assert.True(t, mounts[0].rootedSubtreeDeltaCapable)
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

	mounts, err := buildConfiguredMountSpecs([]*resolvedDriveWithSelection{{
		SelectionIndex: 0,
		Drive:          rd,
	}})
	require.NoError(t, err)

	_, err = engineMountConfigForMount(mounts[0])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}
