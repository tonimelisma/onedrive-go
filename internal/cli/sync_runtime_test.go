package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type testSyncDaemonOrchestrator struct {
	called bool
	mode   syncengine.SyncMode
	opts   syncengine.WatchOptions
	err    error
}

func (o *testSyncDaemonOrchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	_ = ctx
	o.called = true
	o.mode = mode
	o.opts = opts
	return o.err
}

func loadSyncTestHolder(t *testing.T, cfgPath string) *config.Holder {
	t.Helper()

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return config.NewHolder(cfg, cfgPath)
}

// Validates: R-2.1, R-2.10.3
func TestRunSyncWatch_UsesInjectedRunner(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{}
	holder := config.NewHolder(&config.Config{}, "")
	expectedOpts := syncengine.WatchOptions{PollInterval: 2}
	called := false
	cc.syncWatchRunner = func(
		ctx context.Context,
		gotHolder *config.Holder,
		selectors []string,
		mode syncengine.SyncMode,
		opts syncengine.WatchOptions,
		logger *slog.Logger,
		statusWriter io.Writer,
		controlSocketPath string,
	) error {
		called = true
		assert.Equal(t, t.Context(), ctx)
		assert.Same(t, holder, gotHolder)
		assert.Equal(t, []string{"drive-a"}, selectors)
		assert.Equal(t, syncengine.SyncBidirectional, mode)
		assert.Equal(t, expectedOpts, opts)
		assert.NotNil(t, logger)
		assert.Equal(t, io.Discard, statusWriter)
		assert.Equal(t, "/tmp/control.sock", controlSocketPath)
		return nil
	}

	err := runSyncWatch(
		t.Context(),
		cc,
		holder,
		[]string{"drive-a"},
		syncengine.SyncBidirectional,
		expectedOpts,
		slog.New(slog.DiscardHandler),
		io.Discard,
		"/tmp/control.sock",
	)
	require.NoError(t, err)
	assert.True(t, called)
}

// Validates: R-2.1, R-2.10.3
func TestRunSyncOnce_UsesInjectedRunner(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{}
	holder := config.NewHolder(&config.Config{}, "")
	drive := &config.ResolvedDrive{CanonicalID: driveid.MustCanonicalID("personal:sync-once@example.com")}
	expectedResult := multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{
			Results: []multisync.MountStartupResult{{
				Identity: testStandaloneMountIdentity(drive.CanonicalID),
				Status:   multisync.MountStartupRunnable,
			}},
		},
		Reports: []*multisync.MountReport{{Identity: testStandaloneMountIdentity(drive.CanonicalID)}},
	}
	cc.syncRunOnceRunner = func(
		ctx context.Context,
		gotHolder *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		opts syncengine.RunOptions,
		logger *slog.Logger,
		controlSocketPath string,
	) multisync.RunOnceResult {
		assert.Equal(t, t.Context(), ctx)
		assert.Same(t, holder, gotHolder)
		assert.Equal(t, []*config.ResolvedDrive{drive}, drives)
		assert.Equal(t, syncengine.SyncUploadOnly, mode)
		assert.Equal(t, syncengine.RunOptions{DryRun: true}, opts)
		assert.NotNil(t, logger)
		assert.Equal(t, "/tmp/control.sock", controlSocketPath)
		return expectedResult
	}

	result := runSyncOnce(
		t.Context(),
		cc,
		holder,
		[]*config.ResolvedDrive{drive},
		syncengine.SyncUploadOnly,
		syncengine.RunOptions{DryRun: true},
		slog.New(slog.DiscardHandler),
		"/tmp/control.sock",
	)
	assert.Equal(t, expectedResult, result)
}

// Validates: R-2.8.1
func TestStandaloneMountSelectionFromResolvedDrives_PreservesMountBoundaryFields(t *testing.T) {
	t.Parallel()

	first := &config.ResolvedDrive{
		CanonicalID:            driveid.MustCanonicalID("personal:first@example.com"),
		DisplayName:            "First",
		SyncDir:                filepath.Join(t.TempDir(), "first"),
		DriveID:                driveid.New("first-drive"),
		RemoteRootItemID:       "first-root",
		RemoteRootDeltaCapable: true,
		TransfersConfig: config.TransfersConfig{
			TransferWorkers: 7,
			CheckWorkers:    8,
		},
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "3MiB"},
		SyncConfig:   config.SyncConfig{Websocket: true},
	}
	second := &config.ResolvedDrive{
		CanonicalID:  driveid.MustCanonicalID("business:second@example.com"),
		DisplayName:  "Second",
		SyncDir:      filepath.Join(t.TempDir(), "second"),
		DriveID:      driveid.New("second-drive"),
		Paused:       true,
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "0"},
	}

	selection := standaloneMountSelectionFromResolvedDrives([]*config.ResolvedDrive{first, second})
	mounts := selection.Mounts
	require.Len(t, mounts, 2)
	assert.Empty(t, selection.StartupResults)

	assert.Equal(t, 0, mounts[0].SelectionIndex)
	assert.Equal(t, first.CanonicalID, mounts[0].CanonicalID)
	assert.Equal(t, first.DisplayName, mounts[0].DisplayName)
	assert.Equal(t, first.SyncDir, mounts[0].SyncRoot)
	assert.Equal(t, first.StatePath(), mounts[0].StatePath)
	assert.Equal(t, first.DriveID, mounts[0].RemoteDriveID)
	assert.Equal(t, first.RemoteRootItemID, mounts[0].RemoteRootItemID)
	assert.Equal(t, first.CanonicalID, mounts[0].TokenOwnerCanonical)
	assert.Equal(t, first.CanonicalID.Email(), mounts[0].AccountEmail)
	assert.True(t, mounts[0].EnableWebsocket)
	assert.True(t, mounts[0].RemoteRootDeltaCapable)
	assert.Equal(t, 7, mounts[0].TransferWorkers)
	assert.Equal(t, 8, mounts[0].CheckWorkers)
	assert.Equal(t, int64(3*1024*1024), mounts[0].MinFreeSpaceBytes)

	assert.Equal(t, 1, mounts[1].SelectionIndex)
	assert.True(t, mounts[1].Paused)
	assert.False(t, mounts[1].RemoteRootDeltaCapable)
}

// Validates: R-2.8.1
func TestStandaloneMountSelectionFromResolvedDrives_PrefersTokenOwnerAccountEmail(t *testing.T) {
	setTestDriveHome(t)

	sharedCID := driveid.MustCanonicalID("shared:shared@example.com:remote-drive:remote-root")
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.UpsertDrive(&config.CatalogDrive{
			CanonicalID:           sharedCID.String(),
			OwnerAccountCanonical: "personal:owner@example.com",
			DriveType:             sharedCID.DriveType(),
			RemoteDriveID:         "remote-drive",
		})
		return nil
	}))

	drive := &config.ResolvedDrive{
		CanonicalID:      sharedCID,
		DisplayName:      "Shared",
		SyncDir:          t.TempDir(),
		DriveID:          driveid.New("remote-drive"),
		RemoteRootItemID: "remote-root",
	}

	selection := standaloneMountSelectionFromResolvedDrives([]*config.ResolvedDrive{drive})
	require.Len(t, selection.Mounts, 1)
	assert.Empty(t, selection.StartupResults)
	assert.Equal(t, "owner@example.com", selection.Mounts[0].AccountEmail)
	assert.Equal(t, "remote-root", selection.Mounts[0].RemoteRootItemID)
}

// Validates: R-2.8.1
func TestStandaloneMountSelectionFromResolvedDrives_InvalidMinFreeSpaceIsMountLocal(t *testing.T) {
	t.Parallel()

	drive := &config.ResolvedDrive{
		CanonicalID:  driveid.MustCanonicalID("personal:bad-size@example.com"),
		SyncDir:      t.TempDir(),
		DriveID:      driveid.New("bad-size-drive"),
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "not-a-size"},
	}

	selection := standaloneMountSelectionFromResolvedDrives([]*config.ResolvedDrive{drive})
	assert.Empty(t, selection.Mounts)
	require.Len(t, selection.StartupResults, 1)
	assert.Equal(t, 0, selection.StartupResults[0].SelectionIndex)
	assert.Equal(t, testStandaloneMountIdentity(drive.CanonicalID), selection.StartupResults[0].Identity)
	assert.Equal(t, multisync.MountStartupFatal, selection.StartupResults[0].Status)
	require.Error(t, selection.StartupResults[0].Err)
	assert.Contains(t, selection.StartupResults[0].Err.Error(), "invalid min_free_space")
	assert.Contains(t, selection.StartupResults[0].Err.Error(), drive.CanonicalID.String())
}

// Validates: R-2.8.1
func TestStandaloneMountSelectionFromResolvedDrives_TokenOwnerFailureIsMountLocal(t *testing.T) {
	setTestDriveHome(t)

	sharedCID := driveid.MustCanonicalID("shared:missing@example.com:remote-drive:remote-root")
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.UpsertDrive(&config.CatalogDrive{
			CanonicalID:           sharedCID.String(),
			OwnerAccountCanonical: "not-a-canonical-id",
			DriveType:             sharedCID.DriveType(),
			RemoteDriveID:         "remote-drive",
		})
		return nil
	}))

	drive := &config.ResolvedDrive{
		CanonicalID:      sharedCID,
		SyncDir:          t.TempDir(),
		DriveID:          driveid.New("remote-drive"),
		RemoteRootItemID: "remote-root",
	}

	selection := standaloneMountSelectionFromResolvedDrives([]*config.ResolvedDrive{drive})
	assert.Empty(t, selection.Mounts)
	require.Len(t, selection.StartupResults, 1)
	assert.Equal(t, testStandaloneMountIdentity(drive.CanonicalID), selection.StartupResults[0].Identity)
	assert.Equal(t, multisync.MountStartupFatal, selection.StartupResults[0].Status)
	require.Error(t, selection.StartupResults[0].Err)
	assert.Contains(t, selection.StartupResults[0].Err.Error(), "token owner")
}

// Validates: R-2.8.1
func TestReloadStandaloneMountsFunc_UsesWatchSelectors(t *testing.T) {
	t.Parallel()

	firstCID := driveid.MustCanonicalID("personal:first-reload@example.com")
	secondCID := driveid.MustCanonicalID("personal:second-reload@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives[firstCID] = config.Drive{
		SyncDir:     filepath.Join(t.TempDir(), "first"),
		DisplayName: "First",
	}
	cfg.Drives[secondCID] = config.Drive{
		SyncDir:     filepath.Join(t.TempDir(), "second"),
		DisplayName: "Second",
	}

	compile := reloadStandaloneMountsFunc([]string{"Second"}, slog.New(slog.DiscardHandler))
	selection, err := compile(cfg)
	require.NoError(t, err)
	mounts := selection.Mounts
	require.Len(t, mounts, 1)
	assert.Empty(t, selection.StartupResults)
	assert.Equal(t, secondCID, mounts[0].CanonicalID)
}

// Validates: R-2.1, R-2.10.3
func TestRunSyncDaemonWithFactory_NoDrivesConfigured(t *testing.T) {
	t.Parallel()

	holder := config.NewHolder(&config.Config{}, "")

	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		slog.New(slog.DiscardHandler),
		io.Discard,
		"/tmp/control.sock",
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drives configured")
}

// Validates: R-2.1, R-2.10.3
func TestRunSyncDaemonWithFactory_RequiresSyncDir(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:missing-sync-dir@example.com")
	holder := config.NewHolder(&config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			cid: {SyncDir: "relative-sync"},
		},
	}, "")

	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		slog.New(slog.DiscardHandler),
		io.Discard,
		"/tmp/control.sock",
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate drive")
	assert.Contains(t, err.Error(), "sync_dir must be absolute")
}

// Validates: R-2.1, R-2.10.3
func TestRunSyncDaemonWithFactory_CallsOrchestrator(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:watch@example.com")
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))
	holder := loadSyncTestHolder(t, cfgPath)

	orch := &testSyncDaemonOrchestrator{}
	factoryCalls := 0
	logger := slog.New(slog.DiscardHandler)
	opts := syncengine.WatchOptions{PollInterval: 3}
	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		opts,
		logger,
		io.Discard,
		"/tmp/control.sock",
		func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			factoryCalls++
			require.Same(t, holder, cfg.Holder)
			require.Len(t, cfg.StandaloneMounts, 1)
			assert.Empty(t, cfg.InitialStartupResults)
			assert.Equal(t, cid, cfg.StandaloneMounts[0].CanonicalID)
			assert.NotNil(t, cfg.ReloadStandaloneMounts)
			assert.Same(t, logger, cfg.Logger)
			assert.Equal(t, "/tmp/control.sock", cfg.ControlSocketPath)
			assert.NotNil(t, cfg.Runtime)
			return orch
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, factoryCalls)
	assert.True(t, orch.called)
	assert.Equal(t, syncengine.SyncBidirectional, orch.mode)
	assert.Equal(t, opts, orch.opts)
}

func TestRunSyncDaemonWithFactory_CreatesMissingSyncDirBeforeOrchestrator(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:watch@example.com")
	syncDir := filepath.Join(t.TempDir(), "missing", "watch-sync-root")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))
	holder := loadSyncTestHolder(t, cfgPath)

	called := false
	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		slog.New(slog.DiscardHandler),
		io.Discard,
		"/tmp/control.sock",
		func(_ *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			called = true

			info, statErr := os.Stat(syncDir)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())

			return &testSyncDaemonOrchestrator{}
		},
	)
	require.NoError(t, err)
	assert.True(t, called)
}

func TestRunSyncDaemonWithFactory_FormatsResetGuidanceWhenNoMountStarts(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:watch@example.com")
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))
	holder := loadSyncTestHolder(t, cfgPath)

	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		slog.New(slog.DiscardHandler),
		io.Discard,
		"/tmp/control.sock",
		func(_ *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			return &testSyncDaemonOrchestrator{
				err: &multisync.WatchStartupError{
					Summary: multisync.StartupSelectionSummary{
						Results: []multisync.MountStartupResult{{
							Identity: testStandaloneMountIdentity(cid),
							Status:   multisync.MountStartupIncompatibleStore,
							Err: &syncengine.StateStoreIncompatibleError{
								Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
							},
						}},
					},
				},
			}
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive reset-sync-state --drive '"+cid.String()+"'")
}

func TestRunSyncDaemonWithFactory_WarnsWhenSomeDrivesAreSkipped(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:watch@example.com")
	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))
	holder := loadSyncTestHolder(t, cfgPath)

	status := &bytes.Buffer{}
	err := runSyncDaemonWithFactory(
		t.Context(),
		holder,
		nil,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		slog.New(slog.DiscardHandler),
		status,
		"/tmp/control.sock",
		func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			require.NotNil(t, cfg.StartWarning)
			cfg.StartWarning(multisync.StartupWarning{
				Summary: multisync.StartupSelectionSummary{
					Results: []multisync.MountStartupResult{{
						Identity: testStandaloneMountIdentity(cid),
						Status:   multisync.MountStartupIncompatibleStore,
						Err: &syncengine.StateStoreIncompatibleError{
							Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
						},
					}},
				},
			})
			return &testSyncDaemonOrchestrator{
				err: nil,
			}
		},
	)
	require.NoError(t, err)
	assert.Contains(t, status.String(), "pause --drive '"+cid.String()+"'")
	assert.Contains(t, status.String(), "drive reset-sync-state --drive '"+cid.String()+"'")
	assert.Contains(t, status.String(), "--drive selecting only other configured parent drives")
}
