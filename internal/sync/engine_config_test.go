package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type configTestPathConvergenceStub struct {
	waitCalls []string
	targets   []configTestPathConvergenceTarget
}

type configTestPathConvergenceTarget struct {
	driveID    driveid.ID
	rootItemID string
}

func (s *configTestPathConvergenceStub) ForTarget(driveID driveid.ID, rootItemID string) driveops.PathConvergence {
	s.targets = append(s.targets, configTestPathConvergenceTarget{driveID: driveID, rootItemID: rootItemID})
	return s
}

func (s *configTestPathConvergenceStub) WaitPathVisible(_ context.Context, remotePath string) (*graph.Item, error) {
	s.waitCalls = append(s.waitCalls, remotePath)
	return &graph.Item{ID: "visible-item-id"}, nil
}

func (s *configTestPathConvergenceStub) DeleteResolvedPath(_ context.Context, _, _ string) error {
	return nil
}

func (s *configTestPathConvergenceStub) PermanentDeleteResolvedPath(_ context.Context, _, _ string) error {
	return nil
}

func TestNewDriveEngine_PropagatesWatchCapabilities(t *testing.T) {
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
			UseLocalTrash:         true,
			DeleteSafetyThreshold: 42,
			MinFreeSpace:          "1MiB",
		},
		SyncConfig: config.SyncConfig{
			Websocket: true,
		},
	}

	eng, err := NewDriveEngine(t.Context(), session, resolved, DriveEngineOptions{
		Logger:      logger,
		VerifyDrive: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, syncDir, eng.syncRoot)
	assert.Equal(t, session.DriveID, eng.driveID)
	assert.Equal(t, resolved.CanonicalID.DriveType(), eng.driveType)
	assert.Equal(t, "shared-root-id", eng.rootItemID)
	assert.True(t, eng.enableWebsocket)
	assert.NotNil(t, eng.socketIOFetcher)
	assert.NotNil(t, eng.driveVerifier)
	assert.NotNil(t, eng.folderDelta)
	assert.NotNil(t, eng.recursiveLister)
	assert.NotNil(t, eng.permHandler)
	assert.True(t, eng.localFilter.SkipDotfiles)
	assert.True(t, eng.localFilter.SkipSymlinks)
	assert.Equal(t, []string{"vendor"}, eng.localFilter.SkipDirs)
	assert.Equal(t, []string{"*.tmp"}, eng.localFilter.SkipFiles)
	assert.Equal(t, []string{"/Projects/report.txt"}, eng.syncScopeConfig.SyncPaths)
	assert.Equal(t, ".syncignore", eng.syncScopeConfig.IgnoreMarker)
	assert.True(t, eng.localRules.RejectSharePointRootForms)
	assert.NotNil(t, eng.execCfg.trashFunc)
	assert.Equal(t, 3, eng.transferWorkers)
	assert.Equal(t, 4, eng.checkWorkers)
	assert.Equal(t, 42, eng.deleteSafetyThreshold)
	assert.Equal(t, int64(1024*1024), eng.minFreeSpace)
}

func TestNewDriveEngine_InvalidMinFreeSpace(t *testing.T) {
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

	_, err := NewDriveEngine(t.Context(), session, resolved, DriveEngineOptions{
		Logger: logger,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}

func TestNewEngine_PropagatesPathConvergenceFactory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	pathConvergence := &configTestPathConvergenceStub{}
	mock := &engineMockClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "created-folder-id"}, nil
		},
	}

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:                 filepath.Join(t.TempDir(), "test.db"),
		SyncRoot:               syncDir,
		DriveID:                driveid.New("abc123"),
		Fetcher:                mock,
		SocketIOFetcher:        mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		PathConvergenceFactory: pathConvergence,
		Logger:                 logger,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	executor := NewExecution(eng.execCfg, emptyBaseline())
	outcome := executor.ExecuteFolderCreate(t.Context(), &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: synctypes.CreateRemote,
		View:       &PathView{Path: "photos"},
	})
	require.True(t, outcome.Success, "expected remote folder create to succeed: %v", outcome.Error)
	assert.Equal(t, []configTestPathConvergenceTarget{{
		driveID:    driveid.New("abc123"),
		rootItemID: "",
	}}, pathConvergence.targets)
	assert.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}
