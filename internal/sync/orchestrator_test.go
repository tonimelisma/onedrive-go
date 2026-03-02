package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// --- helpers ---

func testOrchestratorConfig(t *testing.T, drives ...*config.ResolvedDrive) *OrchestratorConfig {
	t.Helper()

	return &OrchestratorConfig{
		Config:       config.DefaultConfig(),
		Drives:       drives,
		ConfigPath:   "/tmp/test-config.toml",
		MetaHTTP:     &http.Client{},
		TransferHTTP: &http.Client{},
		UserAgent:    "test/1.0",
		Logger:       slog.Default(),
	}
}

func testResolvedDrive(t *testing.T, cidStr, displayName string) *config.ResolvedDrive {
	t.Helper()

	cid := testCanonicalID(t, cidStr)

	return &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: displayName,
		SyncDir:     t.TempDir(),
		DriveID:     driveid.New("test-drive-id"),
	}
}

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token() (string, error) { return "test-token", nil }

// --- NewOrchestrator ---

func TestNewOrchestrator_ReturnsNonNil(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	require.NotNil(t, orch)
	assert.Equal(t, cfg, orch.cfg)
	assert.NotNil(t, orch.clients)
	assert.NotNil(t, orch.logger)
}

func TestNewOrchestrator_DefaultFactories(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	// engineFactory and tokenSourceFn are set to defaults (non-nil).
	assert.NotNil(t, orch.engineFactory)
	assert.NotNil(t, orch.tokenSourceFn)
}

// --- getOrCreateClient ---

func TestGetOrCreateClient_SamePathReturnsSamePointer(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	pair1, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)
	require.NotNil(t, pair1)

	pair2, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)

	// Same token path should yield the exact same pointer.
	assert.Same(t, pair1, pair2)
}

func TestGetOrCreateClient_DifferentPathReturnsDifferentPointer(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	pair1, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)

	pair2, err := orch.getOrCreateClient(context.Background(), "/tmp/token_b.json")
	require.NoError(t, err)

	assert.NotSame(t, pair1, pair2)
}

func TestGetOrCreateClient_TokenError(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("not logged in")
	}

	pair, err := orch.getOrCreateClient(context.Background(), "/tmp/no-token.json")
	assert.Error(t, err)
	assert.Nil(t, pair)
	assert.Contains(t, err.Error(), "not logged in")
}

// --- RunOnce ---

func TestRunOnce_ZeroDrives(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.Empty(t, reports)
}

func TestRunOnce_OneDrive_Success(t *testing.T) {
	rd := testResolvedDrive(t, "personal:test@example.com", "Test")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	expectedReport := &SyncReport{
		Mode:      SyncBidirectional,
		Downloads: 5,
	}

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{report: expectedReport}, nil
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 1)
	assert.Equal(t, rd.CanonicalID, reports[0].CanonicalID)
	assert.Equal(t, "Test", reports[0].DisplayName)
	assert.NoError(t, reports[0].Err)
	require.NotNil(t, reports[0].Report)
	assert.Equal(t, 5, reports[0].Report.Downloads)
}

func TestRunOnce_TwoDrives_OneFailsOneSucceeds(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:fail@example.com", "Failing")
	rd2 := testResolvedDrive(t, "personal:ok@example.com", "Working")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	errDelta := errors.New("delta gone")
	okReport := &SyncReport{Mode: SyncBidirectional, Uploads: 2}

	orch.engineFactory = func(ecfg *EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{err: errDelta}, nil
		}

		return &mockEngine{report: okReport}, nil
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 2)

	// Find each drive's report by canonical ID.
	var failReport, okDriveReport *DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			failReport = reports[i]
		} else {
			okDriveReport = reports[i]
		}
	}

	require.NotNil(t, failReport)
	assert.ErrorIs(t, failReport.Err, errDelta)
	assert.Nil(t, failReport.Report)

	require.NotNil(t, okDriveReport)
	assert.NoError(t, okDriveReport.Err)
	assert.Equal(t, 2, okDriveReport.Report.Uploads)
}

func TestRunOnce_PanicRecovery(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:panic@example.com", "Panicking")
	rd2 := testResolvedDrive(t, "personal:stable@example.com", "Stable")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	stableReport := &SyncReport{Mode: SyncBidirectional, Downloads: 1}

	orch.engineFactory = func(ecfg *EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{shouldPanic: true}, nil
		}

		return &mockEngine{report: stableReport}, nil
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 2)

	var panicReport, stableDriveReport *DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			panicReport = reports[i]
		} else {
			stableDriveReport = reports[i]
		}
	}

	require.NotNil(t, panicReport)
	assert.Error(t, panicReport.Err)
	assert.Contains(t, panicReport.Err.Error(), "panic")
	assert.Nil(t, panicReport.Report)

	require.NotNil(t, stableDriveReport)
	assert.NoError(t, stableDriveReport.Err)
	assert.Equal(t, 1, stableDriveReport.Report.Downloads)
}

func TestRunOnce_ContextCanceled(t *testing.T) {
	rd := testResolvedDrive(t, "personal:cancel@example.com", "Cancel")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{err: context.Canceled}, nil
	}

	reports := orch.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.Len(t, reports, 1)
	assert.ErrorIs(t, reports[0].Err, context.Canceled)
}

func TestRunOnce_EngineFactoryError(t *testing.T) {
	rd := testResolvedDrive(t, "personal:factory-err@example.com", "FactoryErr")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return nil, errors.New("db init failed")
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 1)
	assert.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "db init failed")
}

func TestRunOnce_TokenError_ReportsPerDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:notoken@example.com", "NoToken")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("token file not found")
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 1)
	assert.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "token")
}

// --- zero DriveID ---

func TestRunOnce_ZeroDriveID_ReportsError(t *testing.T) {
	cid := testCanonicalID(t, "personal:zero-id@example.com")

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: "ZeroID",
		SyncDir:     t.TempDir(),
		// DriveID intentionally zero — should produce an error, not trigger discovery.
	}
	require.True(t, rd.DriveID.IsZero())

	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	reports := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	require.Len(t, reports, 1)
	assert.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "drive ID not resolved")
	assert.Contains(t, reports[0].Err.Error(), "login")
}

// --- mockEngine ---

// mockEngine implements engineRunner for unit tests.
type mockEngine struct {
	report      *SyncReport
	err         error
	shouldPanic bool
	closed      bool
	runWatchFn  func(ctx context.Context, mode SyncMode, opts WatchOpts) error
}

func (m *mockEngine) RunOnce(_ context.Context, _ SyncMode, _ RunOpts) (*SyncReport, error) {
	if m.shouldPanic {
		panic("mock engine panic")
	}

	return m.report, m.err
}

func (m *mockEngine) RunWatch(ctx context.Context, mode SyncMode, opts WatchOpts) error {
	if m.runWatchFn != nil {
		return m.runWatchFn(ctx, mode, opts)
	}

	// Default: block until context is canceled.
	<-ctx.Done()

	return ctx.Err()
}

func (m *mockEngine) Close() error {
	m.closed = true
	return nil
}

// --- RunWatch ---

func TestOrchestrator_RunWatch_SingleDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:watch1@example.com", "Watch1")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfig(t, rd)
	cfg.ConfigPath = cfgPath
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	watchStarted := make(chan struct{})

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for watch to start, then shut down.
	select {
	case <-watchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not start in time")
	}

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_RunWatch_MultiDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:multi1@example.com", "Multi1")
	rd2 := testResolvedDrive(t, "personal:multi2@example.com", "Multi2")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.ConfigPath = cfgPath
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait until both drives have started.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_Reload_AddDrive(t *testing.T) {
	t.Skip("requires E2E test isolation (token metadata in temp dir) — see docs/design/E2E.md")

	rd1 := testResolvedDrive(t, "personal:existing@example.com", "Existing")
	cfgPath := writeTestConfig(t, rd1.CanonicalID)
	sighup := make(chan os.Signal, 1)
	cfg := testOrchestratorConfig(t, rd1)
	cfg.ConfigPath = cfgPath
	cfg.SIGHUPChan = sighup
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for first drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Add a second drive to the config and send SIGHUP.
	rd2CID := driveid.MustCanonicalID("personal:added@example.com")
	writeTestConfigMulti(t, cfgPath, rd1.CanonicalID, rd1.SyncDir, rd2CID, t.TempDir())

	sighup <- os.Interrupt

	// Wait for the second drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_Reload_RemoveDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:keep@example.com", "Keep")
	rd2 := testResolvedDrive(t, "personal:remove@example.com", "Remove")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	sighup := make(chan os.Signal, 1)
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.ConfigPath = cfgPath
	cfg.SIGHUPChan = sighup
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(ecfg *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for both drives to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	// Remove rd2 from config and send SIGHUP.
	writeTestConfigSingle(t, cfgPath, rd1.CanonicalID, rd1.SyncDir)
	sighup <- os.Interrupt

	// Wait for one runner to stop (the removed drive).
	require.Eventually(t, func() bool {
		return stopped.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_Reload_PausedDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:pausetest@example.com", "PauseTest")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	sighup := make(chan os.Signal, 1)
	cfg := testOrchestratorConfig(t, rd)
	cfg.ConfigPath = cfgPath
	cfg.SIGHUPChan = sighup
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Pause the drive and send SIGHUP.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	sighup <- os.Interrupt

	// The drive runner should stop.
	require.Eventually(t, func() bool {
		return stopped.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_Reload_InvalidConfig(t *testing.T) {
	rd := testResolvedDrive(t, "personal:invalidcfg@example.com", "InvalidCfg")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	sighup := make(chan os.Signal, 1)
	cfg := testOrchestratorConfig(t, rd)
	cfg.ConfigPath = cfgPath
	cfg.SIGHUPChan = sighup
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Write invalid TOML and send SIGHUP — should keep old state.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	sighup <- os.Interrupt

	// Give reload time to process — the drive should still be running.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), started.Load(), "drive should still be running after invalid config reload")

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_Reload_TimedPauseExpiry(t *testing.T) {
	t.Skip("requires E2E test isolation (token metadata in temp dir) — see docs/design/E2E.md")

	rd := testResolvedDrive(t, "personal:timedpause@example.com", "TimedPause")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	sighup := make(chan os.Signal, 1)
	cfg := testOrchestratorConfig(t, rd)
	cfg.ConfigPath = cfgPath
	cfg.SIGHUPChan = sighup
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ SyncMode, _ WatchOpts) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, SyncBidirectional, WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Set an already-expired timed pause and send SIGHUP.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", "2000-01-01T00:00:00Z"))
	sighup <- os.Interrupt

	// The old runner should stop, and reload should clear expired pause,
	// then start a new runner.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	// Verify paused keys were cleared from config.
	reloadedCfg, err := config.LoadOrDefault(cfgPath, slog.Default())
	require.NoError(t, err)
	d := reloadedCfg.Drives[rd.CanonicalID]
	assert.Nil(t, d.Paused, "paused should be cleared after timed pause expires")
	assert.Nil(t, d.PausedUntil, "paused_until should be cleared after timed pause expires")

	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not stop in time")
	}
}

func TestOrchestrator_RunWatch_ZeroDrives(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	err := orch.RunWatch(context.Background(), SyncBidirectional, WatchOpts{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no drives")
}

// --- test config helpers ---

// writeTestConfig creates a config file with the given drives using the
// real config API to ensure correct TOML format. Each call creates a fresh
// file (AppendDriveSection creates from template if the file doesn't exist).
func writeTestConfig(t *testing.T, cids ...driveid.CanonicalID) string {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	require.NotEmpty(t, cids, "writeTestConfig requires at least one CID")

	for _, cid := range cids {
		require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))
	}

	return cfgPath
}

// writeTestConfigSingle overwrites a config file with a single drive.
// Removes any existing file first to ensure a clean slate.
func writeTestConfigSingle(t *testing.T, cfgPath string, cid driveid.CanonicalID, syncDir string) {
	t.Helper()

	require.NoError(t, os.Remove(cfgPath))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
}

// writeTestConfigMulti overwrites a config file with two drives.
// Removes any existing file first to ensure a clean slate.
func writeTestConfigMulti(t *testing.T, cfgPath string, cid1 driveid.CanonicalID, dir1 string, cid2 driveid.CanonicalID, dir2 string) {
	t.Helper()

	require.NoError(t, os.Remove(cfgPath))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid1, dir1))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid2, dir2))
}
