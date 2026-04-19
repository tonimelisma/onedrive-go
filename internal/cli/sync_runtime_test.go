package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
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
	mode   syncengine.Mode
	opts   syncengine.WatchOptions
	err    error
}

func (o *testSyncDaemonOrchestrator) RunWatch(ctx context.Context, mode syncengine.Mode, opts syncengine.WatchOptions) error {
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
		mode syncengine.Mode,
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
	expectedReports := []*multisync.DriveReport{{CanonicalID: drive.CanonicalID}}
	cc.syncRunOnceRunner = func(
		ctx context.Context,
		gotHolder *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.Mode,
		opts syncengine.RunOptions,
		logger *slog.Logger,
		controlSocketPath string,
	) []*multisync.DriveReport {
		assert.Equal(t, t.Context(), ctx)
		assert.Same(t, holder, gotHolder)
		assert.Equal(t, []*config.ResolvedDrive{drive}, drives)
		assert.Equal(t, syncengine.SyncUploadOnly, mode)
		assert.Equal(t, syncengine.RunOptions{DryRun: true}, opts)
		assert.NotNil(t, logger)
		assert.Equal(t, "/tmp/control.sock", controlSocketPath)
		return expectedReports
	}

	reports := runSyncOnce(
		t.Context(),
		cc,
		holder,
		[]*config.ResolvedDrive{drive},
		syncengine.SyncUploadOnly,
		syncengine.RunOptions{DryRun: true},
		slog.New(slog.DiscardHandler),
		"/tmp/control.sock",
	)
	assert.Equal(t, expectedReports, reports)
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
			require.Len(t, cfg.Drives, 1)
			assert.Equal(t, cid, cfg.Drives[0].CanonicalID)
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

func TestRunSyncDaemonWithFactory_FormatsResetGuidanceWhenNoDriveStarts(t *testing.T) {
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
					Failures: []multisync.DriveReport{{
						CanonicalID: cid,
						Err: &syncengine.StateDBResetRequiredError{
							Reason: syncengine.StateDBResetReasonIncompatibleSchema,
						},
					}},
				},
			}
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive reset-sync-state --drive "+cid.String())
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
			cfg.StartWarning([]multisync.DriveReport{{
				CanonicalID: cid,
				Err: &syncengine.StateDBResetRequiredError{
					Reason: syncengine.StateDBResetReasonIncompatibleSchema,
				},
			}})
			return &testSyncDaemonOrchestrator{
				err: nil,
			}
		},
	)
	require.NoError(t, err)
	assert.Contains(t, status.String(), "pause --drive "+cid.String())
	assert.Contains(t, status.String(), "drive reset-sync-state --drive "+cid.String())
	assert.Contains(t, status.String(), "--drive selecting only other drives")
}
