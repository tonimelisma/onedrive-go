package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// mockWatchRunner implements watchRunner for testing watchLoop.
type mockWatchRunner struct {
	runWatchFn func(ctx context.Context, mode sync.SyncMode, opts sync.WatchOpts) error
}

func (m *mockWatchRunner) RunWatch(ctx context.Context, mode sync.SyncMode, opts sync.WatchOpts) error {
	return m.runWatchFn(ctx, mode, opts)
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCheckPausedState_NotPaused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.False(t, paused)
	assert.Empty(t, pausedUntil)
}

func TestCheckPausedState_Paused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.True(t, paused)
	assert.Empty(t, pausedUntil)
}

func TestCheckPausedState_TimedPause(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2099-01-01T00:00:00Z"))

	paused, pausedUntil := checkPausedState(cfgPath, cid, logger)
	assert.True(t, paused)
	assert.Equal(t, "2099-01-01T00:00:00Z", pausedUntil)
}

func TestCheckPausedState_MissingConfig(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")

	// Non-existent config path — should default to not paused.
	paused, _ := checkPausedState("/nonexistent/config.toml", cid, logger)
	assert.False(t, paused)
}

func TestCheckPausedState_DriveNotInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	otherCID := driveid.MustCanonicalID("personal:other@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))

	// Query for a drive that doesn't exist in config.
	paused, _ := checkPausedState(cfgPath, otherCID, logger)
	assert.False(t, paused)
}

func TestWaitForResume_SIGHUPUnblocks(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Send SIGHUP asynchronously.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sighup <- os.Signal(nil) // Any value unblocks the channel.
	}()

	err := waitForResume(ctx, sighup, cfgPath, cid, "", logger)
	assert.NoError(t, err)
}

func TestWaitForResume_ContextCancellation(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)
	cid := driveid.MustCanonicalID("personal:test@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	sighup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context asynchronously.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := waitForResume(ctx, sighup, cfgPath, cid, "", logger)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForResume_ExpiredTimerReturnsImmediately(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2000-01-01T00:00:00Z"))

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Already expired — should return immediately and clear config.
	start := time.Now()
	err := waitForResume(ctx, sighup, cfgPath, cid, "2000-01-01T00:00:00Z", logger)
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Less(t, elapsed, 1*time.Second)

	// Config should have paused keys cleared.
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
	assert.Nil(t, d.PausedUntil)
}

func TestWaitForResume_TimerExpiry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	sighup := make(chan os.Signal, 1)
	ctx := context.Background()

	// Set a very short timer (100ms in the future).
	until := time.Now().Add(100 * time.Millisecond).Format(time.RFC3339)

	err := waitForResume(ctx, sighup, cfgPath, cid, until, logger)
	assert.NoError(t, err)
}

func TestDaemonClearPausedKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	logger := testLogger(t)

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", "2099-01-01T00:00:00Z"))

	daemonClearPausedKeys(cfgPath, cid, logger)

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
	assert.Nil(t, d.PausedUntil)
}

// --- watchLoop integration tests ---

// newTestConfig creates a config file with a drive and returns the path and CID.
func newTestConfig(t *testing.T) (cfgPath string, cid driveid.CanonicalID) {
	t.Helper()

	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.toml")
	cid = driveid.MustCanonicalID("personal:test@example.com")

	require.NoError(t, config.CreateConfigWithDrive(cfgPath, cid, "~/OneDrive"))

	return cfgPath, cid
}

func TestWatchLoop_ParentContextShutdown(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			// Block until context is canceled (simulates normal RunWatch).
			<-ctx.Done()

			return ctx.Err()
		},
	}

	// Cancel parent context after RunWatch starts.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	assert.NoError(t, err)
}

func TestWatchLoop_SIGHUPRestartsRunWatch(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var callCount atomic.Int32
	// Signal that the second RunWatch call has started.
	secondCallStarted := make(chan struct{})

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			n := callCount.Add(1)
			if n == 2 {
				close(secondCallStarted)
			}

			<-ctx.Done()

			return ctx.Err()
		},
	}

	// Orchestrator goroutine: send SIGHUP after first RunWatch starts,
	// then cancel parent ctx after second RunWatch starts.
	go func() {
		time.Sleep(100 * time.Millisecond)
		sighup <- os.Interrupt // any signal value works

		<-secondCallStarted
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, callCount.Load(), int32(2), "RunWatch should be called at least twice")
}

func TestWatchLoop_PausedThenSIGHUPResumes(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	// Start paused.
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWatchCalled := make(chan struct{})

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			close(runWatchCalled)
			<-ctx.Done()

			return ctx.Err()
		},
	}

	// Orchestrator: after a short delay, unpause config and send SIGHUP.
	// Then cancel parent ctx once RunWatch starts.
	go func() {
		time.Sleep(100 * time.Millisecond)

		// Remove paused state from config before sending SIGHUP.
		_ = config.DeleteDriveKey(cfgPath, cid, "paused")

		sighup <- os.Interrupt

		<-runWatchCalled
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	assert.NoError(t, err)

	// Verify RunWatch was actually called (drive was resumed).
	select {
	case <-runWatchCalled:
		// good
	default:
		t.Fatal("RunWatch was never called after resuming")
	}
}

func TestWatchLoop_SIGHUPWhilePausedConfigStillPaused(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	// Start paused.
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			t.Fatal("RunWatch should not be called while drive is paused")

			return nil
		},
	}

	// Send SIGHUP (config still paused) → loop re-checks, still paused →
	// blocks again in waitForResume → cancel parent ctx.
	go func() {
		time.Sleep(100 * time.Millisecond)
		sighup <- os.Interrupt // config still paused, loop re-enters waitForResume

		time.Sleep(100 * time.Millisecond)
		cancel() // shut down
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWatchLoop_TimedPauseAutoResumes(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	// Start paused with a very short timed pause.
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	until := time.Now().Add(200 * time.Millisecond).Format(time.RFC3339)
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused_until", until))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWatchCalled := make(chan struct{})

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			close(runWatchCalled)
			<-ctx.Done()

			return ctx.Err()
		},
	}

	// Cancel parent ctx once RunWatch starts.
	go func() {
		<-runWatchCalled
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	assert.NoError(t, err)

	// Verify paused keys were cleared from config by the daemon.
	cfg, loadErr := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, loadErr)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused, "paused should be cleared after timed pause expires")
	assert.Nil(t, d.PausedUntil, "paused_until should be cleared after timed pause expires")
}

func TestWatchLoop_SIGHUPPausesRunningDrive(t *testing.T) {
	t.Parallel()

	cfgPath, cid := newTestConfig(t)
	logger := testLogger(t)
	sighup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWatchStarted := make(chan struct{}, 1)

	runner := &mockWatchRunner{
		runWatchFn: func(ctx context.Context, _ sync.SyncMode, _ sync.WatchOpts) error {
			runWatchStarted <- struct{}{}
			<-ctx.Done()

			return ctx.Err()
		},
	}

	// Orchestrator: wait for RunWatch to start, then pause the drive and
	// send SIGHUP. The loop should re-read config, see paused=true, and
	// enter waitForResume. Then cancel parent ctx.
	go func() {
		<-runWatchStarted
		time.Sleep(50 * time.Millisecond)

		_ = config.SetDriveKey(cfgPath, cid, "paused", "true")
		sighup <- os.Interrupt

		// Give the loop time to re-read config and enter waitForResume.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := watchLoop(ctx, runner, sync.SyncBidirectional, sync.WatchOpts{}, cfgPath, cid, sighup, logger)
	// watchLoop should return context.Canceled from waitForResume since the
	// drive is now paused and we canceled the parent context.
	assert.ErrorIs(t, err, context.Canceled)
}

// --- driveReportsError ---

func TestDriveReportsError(t *testing.T) {
	t.Parallel()

	errDelta := fmt.Errorf("delta expired")
	errAuth := fmt.Errorf("auth failed")

	tests := []struct {
		name    string
		reports []*sync.DriveReport
		wantNil bool
		wantMsg string // substring, only checked when wantNil is false
	}{
		{
			name:    "zero reports",
			reports: nil,
			wantNil: true,
		},
		{
			name: "one success",
			reports: []*sync.DriveReport{
				{Report: &sync.SyncReport{Mode: sync.SyncBidirectional}},
			},
			wantNil: true,
		},
		{
			name: "one failure",
			reports: []*sync.DriveReport{
				{Err: errDelta},
			},
			wantMsg: "delta expired",
		},
		{
			name: "multi-drive mixed",
			reports: []*sync.DriveReport{
				{Report: &sync.SyncReport{}},
				{Err: errDelta},
			},
			wantMsg: "1 of 2 drives failed",
		},
		{
			name: "all failures",
			reports: []*sync.DriveReport{
				{Err: errDelta},
				{Err: errAuth},
			},
			wantMsg: "2 of 2 drives failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := driveReportsError(tt.reports)
			if tt.wantNil {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
		})
	}
}

// --- printDriveReports ---

func TestPrintDriveReports_SingleDrive_NoHeader(t *testing.T) {
	t.Parallel()

	reports := []*sync.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &sync.SyncReport{Mode: sync.SyncBidirectional},
		},
	}

	// Should not panic or produce headers for single drive.
	printDriveReports(reports, true)
}

func TestPrintDriveReports_MultiDrive_WithError(t *testing.T) {
	t.Parallel()

	reports := []*sync.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &sync.SyncReport{Mode: sync.SyncBidirectional},
		},
		{
			DisplayName: "Business",
			Err:         fmt.Errorf("sync failed"),
		},
	}

	// Should not panic. Output goes to stderr via statusf.
	printDriveReports(reports, true)
}
