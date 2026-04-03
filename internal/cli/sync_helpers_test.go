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
			UseLocalTrash:      true,
			BigDeleteThreshold: 42,
			MinFreeSpace:       "1MiB",
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

	ecfg, err := buildSyncEngineConfig(session, resolved, false, logger)
	require.NoError(t, err)
	require.NotNil(t, ecfg)
	assert.True(t, ecfg.LocalFilter.SkipDotfiles)
	assert.True(t, ecfg.LocalFilter.SkipSymlinks)
	assert.Equal(t, []string{"vendor"}, ecfg.LocalFilter.SkipDirs)
	assert.Equal(t, []string{"*.log"}, ecfg.LocalFilter.SkipFiles)
}
