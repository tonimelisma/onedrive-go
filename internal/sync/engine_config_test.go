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
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type configTestPathConvergenceStub struct {
	waitCalls []string
}

func testEngineMountConfig(syncDir string) *EngineMountConfig {
	return &EngineMountConfig{
		DBPath:                 filepath.Join(filepath.Dir(syncDir), "state.db"),
		SyncRoot:               syncDir,
		DataDir:                config.DefaultDataDir(),
		DriveID:                driveid.New("mount-drive-id"),
		DriveType:              driveid.MustCanonicalID("sharepoint:test@example.com:site:Documents").DriveType(),
		AccountEmail:           "mount-user@example.com",
		RemoteRootItemID:       "mount-root-id",
		RemoteRootDeltaCapable: false,
		EnableWebsocket:        true,
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
		Meta:              &graph.Client{},
		Transfer:          &graph.Client{},
		DriveID:           driveid.New("session-drive-id"),
		AccountEmailValue: "session@example.com",
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
	mountCfg.RemoteRootItemID = ""
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
	assert.Empty(t, eng.remoteRootItemID)
	assert.False(t, eng.enableWebsocket)
	assert.False(t, eng.localRules.RejectSharePointRootForms)
	assert.Equal(t, mountCfg.AccountEmail, eng.permHandler.accountEmail)
	assert.Equal(t, mountCfg.TransferWorkers, eng.transferWorkers)
	assert.Equal(t, mountCfg.CheckWorkers, eng.checkWorkers)
	assert.Equal(t, mountCfg.MinFreeSpace, eng.minFreeSpace)
	assert.Nil(t, eng.driveVerifier)
}

// Validates: R-2.4.8
func TestNewMountEngine_LoadsPersistedShortcutProtectedRoots(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	mountCfg := testEngineMountConfig(syncDir)
	mountCfg.ShortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID

	store, err := openEngineSyncStore(t.Context(), mountCfg.DBPath, logger)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs", "Shared/Old Docs"},
		LocalRootIdentity: &synctree.FileIdentity{Device: 7, Inode: 9},
	}}))
	require.NoError(t, store.Close(t.Context()))

	eng, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, logger, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	require.Len(t, eng.protectedRoots, 2)
	assert.Equal(t, "Shared/Docs", eng.protectedRoots[0].Path)
	assert.Equal(t, "binding-1", eng.protectedRoots[0].BindingID)
	assert.True(t, eng.protectedRoots[0].HasIdentity)
	assert.Equal(t, "Shared/Old Docs", eng.protectedRoots[1].Path)
}

// Validates: R-2.4.8
func TestNewMountEngine_DoesNotProtectReleasedShortcutRootAfterDrainAck(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	logger := testLogger(t)
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	mountCfg := testEngineMountConfig(syncDir)
	mountCfg.ShortcutTopologyNamespaceID = shortcutTopologyTestNamespaceID

	store, err := openEngineSyncStore(t.Context(), mountCfg.DBPath, logger)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutTopologyTestNamespaceID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateRemovedFinalDrain,
		ProtectedPaths:    []string{"Shared/Docs"},
	}}))
	changed, err := store.AcknowledgeShortcutChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		BindingItemID: "binding-1",
	})
	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, store.Close(t.Context()))

	eng, err := NewMountEngine(t.Context(), testEngineSession(), mountCfg, logger, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Empty(t, eng.protectedRoots)
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
	assert.Equal(t, mountCfg.RemoteRootItemID, eng.remoteRootItemID)
	assert.False(t, eng.remoteRootDeltaCapable)
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
	mountCfg.RemoteRootItemID = "mount-owned-root-id"
	mountCfg.EnableWebsocket = false
	mountCfg.LocalRules = LocalObservationRules{}

	eng, err := NewMountEngine(t.Context(), session, mountCfg, logger, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	assert.Equal(t, mountCfg.DriveID, eng.driveID)
	assert.Equal(t, mountCfg.DriveType, eng.driveType)
	assert.Equal(t, mountCfg.RemoteRootItemID, eng.remoteRootItemID)
	assert.Equal(t, mountCfg.AccountEmail, eng.permHandler.accountEmail)
	assert.False(t, eng.enableWebsocket)
	assert.False(t, eng.localRules.RejectSharePointRootForms)
	assert.NotEqual(t, session.DriveID, eng.driveID)
	assert.NotEqual(t, session.AccountEmail(), eng.permHandler.accountEmail)

	mountSession, ok := eng.execCfg.pathConvergence.(*driveops.MountSession)
	require.True(t, ok)
	assert.Equal(t, mountCfg.RemoteRootItemID, mountSession.RemoteRootItemID)
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
