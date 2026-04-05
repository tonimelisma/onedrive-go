package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestBuildEngineConfig_PropagatesWatchCapabilities(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     &graph.Client{},
		Transfer: &graph.Client{},
		DriveID:  driveid.New("abc123"),
		RootItem: "shared-root-id",
	}
	resolved := &config.ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents"),
		SyncDir:     syncDir,
		RootItemID:  "shared-root-id",
		FilterConfig: config.FilterConfig{
			SkipDotfiles: true,
			SkipSymlinks: true,
			SkipDirs:     []string{"vendor"},
			SkipFiles:    []string{"*.tmp"},
			SyncPaths:    []string{"/Projects/report.txt"},
			IgnoreMarker: ".syncignore",
		},
		TransfersConfig: config.TransfersConfig{
			TransferWorkers: 3,
			CheckWorkers:    4,
		},
		SafetyConfig: config.SafetyConfig{
			UseLocalTrash:      true,
			BigDeleteThreshold: 42,
			MinFreeSpace:       "1MiB",
		},
		SyncConfig: config.SyncConfig{
			Websocket: true,
		},
	}

	ecfg, err := BuildEngineConfig(session, resolved, true, logger)
	require.NoError(t, err)

	assert.Equal(t, syncDir, ecfg.SyncRoot)
	assert.Equal(t, resolved.StatePath(), ecfg.DBPath)
	assert.Equal(t, session.DriveID, ecfg.DriveID)
	assert.Equal(t, resolved.CanonicalID.DriveType(), ecfg.DriveType)
	assert.Equal(t, resolved.CanonicalID.Email(), ecfg.AccountEmail)
	assert.Equal(t, "shared-root-id", ecfg.RootItemID)
	assert.True(t, ecfg.EnableWebsocket)
	assert.NotNil(t, ecfg.SocketIOFetcher)
	assert.NotNil(t, ecfg.DriveVerifier)
	assert.NotNil(t, ecfg.FolderDelta)
	assert.NotNil(t, ecfg.RecursiveLister)
	assert.NotNil(t, ecfg.PermChecker)
	assert.True(t, ecfg.LocalFilter.SkipDotfiles)
	assert.True(t, ecfg.LocalFilter.SkipSymlinks)
	assert.Equal(t, []string{"vendor"}, ecfg.LocalFilter.SkipDirs)
	assert.Equal(t, []string{"*.tmp"}, ecfg.LocalFilter.SkipFiles)
	assert.Equal(t, []string{"/Projects/report.txt"}, ecfg.SyncScope.SyncPaths)
	assert.Equal(t, ".syncignore", ecfg.SyncScope.IgnoreMarker)
	assert.True(t, ecfg.LocalRules.RejectSharePointRootForms)
	assert.True(t, ecfg.UseLocalTrash)
	assert.Equal(t, 3, ecfg.TransferWorkers)
	assert.Equal(t, 4, ecfg.CheckWorkers)
	assert.Equal(t, 42, ecfg.BigDeleteThreshold)
	assert.Equal(t, int64(1024*1024), ecfg.MinFreeSpace)
}

func TestBuildEngineConfig_InvalidMinFreeSpace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)

	session := &driveops.Session{
		Meta:     &graph.Client{},
		Transfer: &graph.Client{},
		DriveID:  driveid.New("abc123"),
	}
	resolved := &config.ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
		SyncDir:     t.TempDir(),
		SafetyConfig: config.SafetyConfig{
			MinFreeSpace: "not-a-size",
		},
	}

	_, err := BuildEngineConfig(session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}
