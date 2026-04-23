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
)

type configTestPathConvergenceStub struct {
	waitCalls []string
}

func testEngineMountConfig(syncDir string) *EngineMountConfig {
	return &EngineMountConfig{
		DBPath:                    filepath.Join(filepath.Dir(syncDir), "state.db"),
		SyncRoot:                  syncDir,
		DataDir:                   config.DefaultDataDir(),
		DriveID:                   driveid.New("mount-drive-id"),
		DriveType:                 driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents").DriveType(),
		AccountEmail:              "mount-user@example.com",
		RootItemID:                "mount-root-id",
		RootedSubtreeDeltaCapable: false,
		EnableWebsocket:           true,
		LocalFilter:               LocalFilterConfig{},
		LocalRules: LocalObservationRules{
			RejectSharePointRootForms: true,
		},
		TransferWorkers: 3,
		CheckWorkers:    4,
		MinFreeSpace:    1024 * 1024,
	}
}

func testEngineSession() *driveops.Session {
	return &driveops.Session{
		Meta:     &graph.Client{},
		Transfer: &graph.Client{},
		DriveID:  driveid.New("session-drive-id"),
		RootItem: "session-root-id",
		Resolved: &config.ResolvedDrive{
			CanonicalID: driveid.MustCanonicalID("personal:session@example.com"),
			RootItemID:  "session-root-id",
		},
	}
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

func TestNewMountEngine_PropagatesOrdinaryMountConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	mountCfg := testEngineMountConfig(syncDir)
	mountCfg.DriveType = driveid.MustCanonicalID("personal:test@example.com").DriveType()
	mountCfg.RootItemID = ""
	mountCfg.EnableWebsocket = false
	mountCfg.LocalRules = LocalObservationRules{}

	eng, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, logger, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, mountCfg.SyncRoot, eng.syncRoot)
	assert.Equal(t, mountCfg.DriveID, eng.driveID)
	assert.Equal(t, mountCfg.DriveType, eng.driveType)
	assert.Empty(t, eng.rootItemID)
	assert.False(t, eng.enableWebsocket)
	assert.False(t, eng.localRules.RejectSharePointRootForms)
	assert.Equal(t, mountCfg.AccountEmail, eng.permHandler.accountEmail)
	assert.Equal(t, mountCfg.TransferWorkers, eng.transferWorkers)
	assert.Equal(t, mountCfg.CheckWorkers, eng.checkWorkers)
	assert.Equal(t, mountCfg.MinFreeSpace, eng.minFreeSpace)
	assert.Nil(t, eng.driveVerifier)
}

func TestNewMountEngine_PropagatesRootedMountConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	mountCfg := testEngineMountConfig(syncDir)

	eng, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, logger, nil, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, mountCfg.SyncRoot, eng.syncRoot)
	assert.Equal(t, mountCfg.DriveID, eng.driveID)
	assert.Equal(t, mountCfg.DriveType, eng.driveType)
	assert.Equal(t, mountCfg.RootItemID, eng.rootItemID)
	assert.False(t, eng.rootedSubtreeDeltaCapable)
	assert.True(t, eng.enableWebsocket)
	assert.NotNil(t, eng.socketIOFetcher)
	assert.NotNil(t, eng.driveVerifier)
	assert.NotNil(t, eng.folderDelta)
	assert.NotNil(t, eng.recursiveLister)
	assert.NotNil(t, eng.permHandler)
	assert.Equal(t, mountCfg.AccountEmail, eng.permHandler.accountEmail)
	assert.True(t, eng.localRules.RejectSharePointRootForms)
	assert.Equal(t, mountCfg.TransferWorkers, eng.transferWorkers)
	assert.Equal(t, mountCfg.CheckWorkers, eng.checkWorkers)
	assert.Equal(t, mountCfg.MinFreeSpace, eng.minFreeSpace)
}

func TestNewMountEngine_UsesMountConfigNotSessionResolvedFields(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := testEngineSession()
	mountCfg := testEngineMountConfig(syncDir)
	mountCfg.DriveID = driveid.New("mount-owned-drive-id")
	mountCfg.DriveType = driveid.MustCanonicalID("business:mount@example.com").DriveType()
	mountCfg.AccountEmail = "mount-owner@example.com"
	mountCfg.RootItemID = "mount-owned-root-id"
	mountCfg.EnableWebsocket = false
	mountCfg.LocalRules = LocalObservationRules{}

	eng, err := NewMountEngine(t.Context(), session, mountCfg, logger, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, mountCfg.DriveID, eng.driveID)
	assert.Equal(t, mountCfg.DriveType, eng.driveType)
	assert.Equal(t, mountCfg.RootItemID, eng.rootItemID)
	assert.Equal(t, mountCfg.AccountEmail, eng.permHandler.accountEmail)
	assert.False(t, eng.enableWebsocket)
	assert.False(t, eng.localRules.RejectSharePointRootForms)
	assert.NotEqual(t, session.DriveID, eng.driveID)
	assert.NotEqual(t, session.RootItem, eng.rootItemID)
	assert.NotEqual(t, session.Resolved.CanonicalID.Email(), eng.permHandler.accountEmail)
}

func TestNewMountEngine_RequiresSyncRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountCfg := testEngineMountConfig(t.TempDir())
	mountCfg.SyncRoot = ""

	_, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, testLogger(t), nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync root is required")
}

func TestNewMountEngine_RequiresDBPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountCfg := testEngineMountConfig(t.TempDir())
	mountCfg.DBPath = ""

	_, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, testLogger(t), nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state DB path is required")
}

func TestNewMountEngine_RequiresDriveID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountCfg := testEngineMountConfig(t.TempDir())
	mountCfg.DriveID = driveid.ID{}

	_, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, testLogger(t), nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive ID is required")
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
		RootItem: "rooted-subtree-id",
	}
	resolved := &config.ResolvedDrive{
		CanonicalID:            driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents"),
		SyncDir:                syncDir,
		DriveID:                session.DriveID,
		RootItemID:             "rooted-subtree-id",
		SharedRootDeltaCapable: false,
		TransfersConfig: config.TransfersConfig{
			TransferWorkers: 3,
			CheckWorkers:    4,
		},
		SafetyConfig: config.SafetyConfig{
			MinFreeSpace: "1MiB",
		},
		SyncConfig: config.SyncConfig{
			Websocket: true,
		},
	}

	eng, err := NewDriveEngine(t.Context(), session, resolved, logger, nil, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, syncDir, eng.syncRoot)
	assert.Equal(t, resolved.DriveID, eng.driveID)
	assert.Equal(t, resolved.CanonicalID.DriveType(), eng.driveType)
	assert.Equal(t, "rooted-subtree-id", eng.rootItemID)
	assert.False(t, eng.rootedSubtreeDeltaCapable)
	assert.True(t, eng.enableWebsocket)
	assert.NotNil(t, eng.socketIOFetcher)
	assert.NotNil(t, eng.driveVerifier)
	assert.NotNil(t, eng.folderDelta)
	assert.NotNil(t, eng.recursiveLister)
	assert.NotNil(t, eng.permHandler)
	assert.True(t, eng.localRules.RejectSharePointRootForms)
	assert.Equal(t, 3, eng.transferWorkers)
	assert.Equal(t, 4, eng.checkWorkers)
	assert.Equal(t, int64(1024*1024), eng.minFreeSpace)
}

func TestNewDriveEngine_WrapsMountConstructorEquivalently(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	session := testEngineSession()
	session.DriveID = driveid.New("resolved-drive-id")

	resolved := &config.ResolvedDrive{
		CanonicalID:            driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents"),
		SyncDir:                syncDir,
		DriveID:                session.DriveID,
		RootItemID:             "rooted-subtree-id",
		SharedRootDeltaCapable: false,
		TransfersConfig: config.TransfersConfig{
			TransferWorkers: 5,
			CheckWorkers:    6,
		},
		SafetyConfig: config.SafetyConfig{
			MinFreeSpace: "2MiB",
		},
		SyncConfig: config.SyncConfig{
			Websocket: true,
		},
	}

	driveEngine, err := NewDriveEngine(t.Context(), session, resolved, logger, nil, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, driveEngine.Close(t.Context()))
	})

	mountEngine, err := NewMountEngine(t.Context(), session, &EngineMountConfig{
		DBPath:                    resolved.StatePath(),
		SyncRoot:                  resolved.SyncDir,
		DataDir:                   config.DefaultDataDir(),
		DriveID:                   resolved.DriveID,
		DriveType:                 resolved.CanonicalID.DriveType(),
		AccountEmail:              resolved.CanonicalID.Email(),
		RootItemID:                resolved.RootItemID,
		RootedSubtreeDeltaCapable: resolved.SharedRootDeltaCapable,
		EnableWebsocket:           resolved.Websocket,
		LocalFilter:               LocalFilterConfig{},
		LocalRules: LocalObservationRules{
			RejectSharePointRootForms: resolved.CanonicalID.IsSharePoint(),
		},
		TransferWorkers: resolved.TransferWorkers,
		CheckWorkers:    resolved.CheckWorkers,
		MinFreeSpace:    2 * 1024 * 1024,
	}, logger, nil, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mountEngine.Close(t.Context()))
	})

	assert.Equal(t, driveEngine.syncRoot, mountEngine.syncRoot)
	assert.Equal(t, driveEngine.driveID, mountEngine.driveID)
	assert.Equal(t, driveEngine.driveType, mountEngine.driveType)
	assert.Equal(t, driveEngine.rootItemID, mountEngine.rootItemID)
	assert.Equal(t, driveEngine.rootedSubtreeDeltaCapable, mountEngine.rootedSubtreeDeltaCapable)
	assert.Equal(t, driveEngine.enableWebsocket, mountEngine.enableWebsocket)
	assert.Equal(t, driveEngine.transferWorkers, mountEngine.transferWorkers)
	assert.Equal(t, driveEngine.checkWorkers, mountEngine.checkWorkers)
	assert.Equal(t, driveEngine.minFreeSpace, mountEngine.minFreeSpace)
	assert.Equal(t, driveEngine.localRules, mountEngine.localRules)
	assert.Equal(t, driveEngine.permHandler.accountEmail, mountEngine.permHandler.accountEmail)
	assert.NotNil(t, mountEngine.driveVerifier)
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
		DriveID:     session.DriveID,
		SafetyConfig: config.SafetyConfig{
			MinFreeSpace: "not-a-size",
		},
	}

	_, err := NewDriveEngine(t.Context(), session, resolved, logger, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min_free_space")
}

func TestNewEngine_PropagatesPathConvergence(t *testing.T) {
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
		DBPath:          filepath.Join(t.TempDir(), "test.db"),
		SyncRoot:        syncDir,
		DriveID:         driveid.New("abc123"),
		Fetcher:         mock,
		SocketIOFetcher: mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		PathConvergence: pathConvergence,
		Logger:          logger,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	executor := NewExecution(eng.execCfg, emptyBaseline())
	outcome := executor.ExecuteFolderCreate(t.Context(), &Action{
		Type:       ActionFolderCreate,
		Path:       "photos",
		CreateSide: CreateRemote,
		View:       &PathView{Path: "photos"},
	})
	require.True(t, outcome.Success, "expected remote folder create to succeed: %v", outcome.Error)
	assert.Equal(t, []string{"photos"}, pathConvergence.waitCalls)
}
