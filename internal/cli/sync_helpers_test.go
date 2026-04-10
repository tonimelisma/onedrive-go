package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestNewSyncEngine_EmptySyncDir(t *testing.T) {
	session := &driveops.Session{DriveID: driveid.New("abc123")}
	resolved := &config.ResolvedDrive{
		SyncDir:     "",
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
	}
	logger := buildLogger(nil, CLIFlags{})

	_, err := newSyncEngine(t.Context(), session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir not configured")
}

func TestNewSyncEngine_EmptyStatePath(t *testing.T) {
	session := &driveops.Session{DriveID: driveid.New("abc123")}
	// A zero CanonicalID produces empty StatePath.
	resolved := &config.ResolvedDrive{
		SyncDir:     "/tmp/sync",
		CanonicalID: driveid.CanonicalID{},
	}
	logger := buildLogger(nil, CLIFlags{})

	_, err := newSyncEngine(t.Context(), session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state DB path")
}

func TestNewSyncEngine_InvalidMinFreeSpace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
	}
	resolved := &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
		SafetyConfig: config.SafetyConfig{
			MinFreeSpace: "not-a-size",
		},
	}

	_, err = newSyncEngine(t.Context(), session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}

func TestNewSyncEngine_Success(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
	}
	resolved := &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
		TransfersConfig: config.TransfersConfig{
			TransferWorkers: 2,
			CheckWorkers:    3,
		},
		SafetyConfig: config.SafetyConfig{
			UseLocalTrash:         true,
			DeleteSafetyThreshold: 42,
			MinFreeSpace:          "1MiB",
		},
	}

	engine, err := newSyncEngine(t.Context(), session, resolved, true, logger)
	require.NoError(t, err)
	require.NotNil(t, engine)
	require.NoError(t, engine.Close(t.Context()))
}

func TestBuildSyncEngineConfig_PropagatesLocalFilters(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
	}
	resolved := &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
		FilterConfig: config.FilterConfig{
			SkipDotfiles: true,
			SkipSymlinks: true,
			SkipDirs:     []string{"vendor"},
			SkipFiles:    []string{"*.log"},
		},
	}

	ecfg, err := syncengine.BuildEngineConfig(session, resolved, false, logger)
	require.NoError(t, err)
	require.NotNil(t, ecfg)
	assert.True(t, ecfg.LocalFilter.SkipDotfiles)
	assert.True(t, ecfg.LocalFilter.SkipSymlinks)
	assert.Equal(t, []string{"vendor"}, ecfg.LocalFilter.SkipDirs)
	assert.Equal(t, []string{"*.log"}, ecfg.LocalFilter.SkipFiles)
}

func TestBuildSyncEngineConfig_SharePointEnablesRootFormsValidation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
	}

	personal, err := syncengine.BuildEngineConfig(session, &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
	}, false, logger)
	require.NoError(t, err)
	assert.False(t, personal.LocalRules.RejectSharePointRootForms)

	sharePoint, err := syncengine.BuildEngineConfig(session, &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents"),
	}, false, logger)
	require.NoError(t, err)
	assert.True(t, sharePoint.LocalRules.RejectSharePointRootForms)
}

func TestBuildSyncEngineConfig_PropagatesSharedRootAndScopedHelpers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
		RootItem: "shared-root-id",
	}
	resolved := &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("shared:test@example.com:b!drive:shared-root-id"),
		RootItemID:  "shared-root-id",
	}

	ecfg, err := syncengine.BuildEngineConfig(session, resolved, true, logger)
	require.NoError(t, err)
	assert.Equal(t, "test@example.com", ecfg.AccountEmail)
	assert.Equal(t, "shared-root-id", ecfg.RootItemID)
	assert.NotNil(t, ecfg.DriveVerifier)
	assert.NotNil(t, ecfg.FolderDelta)
	assert.NotNil(t, ecfg.RecursiveLister)
	assert.NotNil(t, ecfg.PermChecker)
}

func TestBuildSyncEngineConfig_ThreadsWebsocketConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := buildLogger(nil, CLIFlags{})
	meta, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)
	transfer, err := newGraphClient(staticTokenSource{}, logger)
	require.NoError(t, err)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := &driveops.Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  driveid.New("abc123"),
	}
	resolved := &config.ResolvedDrive{
		SyncDir:     syncDir,
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
		SyncConfig: config.SyncConfig{
			Websocket: true,
		},
	}

	ecfg, err := syncengine.BuildEngineConfig(session, resolved, false, logger)
	require.NoError(t, err)
	assert.True(t, ecfg.EnableWebsocket)
	assert.NotNil(t, ecfg.SocketIOFetcher)
}
