package multisync

import (
	"context"
	"log/slog"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.8.12
//
// `pause 30s` writes paused_until into the config. Nothing used to re-read
// the config when that time came, so the drive stayed paused until a
// `resume` or an unrelated reload (seen live on 2026-11-02: a one-minute
// pause still held three minutes later). The watch loop now arms a wake-up
// for the earliest timed pause; the reload it triggers clears the expired
// pause from the config and starts the drive.
func TestRunWatch_TimedPauseExpiresWithoutAReload(t *testing.T) {
	rd := testStandaloneMount(t, "personal:timed-pause@example.com", "TimedPause")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	// The pause ends in the wall clock's future (config loading reads an
	// already-past paused_until as expired), while the orchestrator runs on
	// the test clock, which the test moves past that end.
	clock := newTestClock()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	wait := until.Sub(clock.now()) + pauseExpirySlack
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", until.Format(time.RFC3339)))

	capture, logger := newLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	// The reload compiles the drive from the config file as production does;
	// here the stubbed test mount stands in, taking only its pause state from
	// the file, which the reload has just cleared.
	cfg.ReloadStandaloneMounts = func(loaded *config.Config) (StandaloneMountSelection, error) {
		mount := rd
		drive := loaded.Drives[rd.CanonicalID]
		mount.Paused = drive.IsPaused(clock.now())

		return StandaloneMountSelection{Mounts: []StandaloneMountConfig{mount}}, nil
	}
	orch := NewOrchestrator(cfg)
	// The production holder carries the loaded file; the test helper's holds
	// defaults, so give it the file with the pause in it.
	reloadTestOrchestratorConfig(t, orch, cfgPath)
	orch.now = clock.now
	orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

	fire := make(chan time.Time, 1)
	var armedMu gosync.Mutex
	var armed []time.Duration
	orch.restartTimer = func(delay time.Duration) (<-chan time.Time, func()) {
		armedMu.Lock()
		armed = append(armed, delay)
		armedMu.Unlock()
		return fire, func() {}
	}

	started := make(chan struct{})
	var startOnce gosync.Once
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				startOnce.Do(func() { close(started) })
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	require.Eventually(t, func() bool {
		armedMu.Lock()
		defer armedMu.Unlock()
		return len(armed) > 0
	}, 5*time.Second, 5*time.Millisecond, "the pause-expiry wake-up was not armed")
	armedMu.Lock()
	assert.Equal(t, wait, armed[0], "armed for the pause's end")
	armedMu.Unlock()
	select {
	case <-started:
		require.FailNow(t, "the drive must not run while its pause holds")
	default:
	}

	clock.advance(wait)
	fire <- clock.now()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		for _, record := range capture.atLeast(slog.LevelDebug) {
			t.Logf("log: %s %v", record.message, record.attrs)
		}
		require.FailNow(t, "the drive did not start when its timed pause expired")
	}

	loaded, err := config.LoadOrDefault(cfgPath, logger)
	require.NoError(t, err)
	drive := loaded.Drives[rd.CanonicalID]
	assert.Nil(t, drive.Paused, "the expired pause is cleared from the config")
	assert.Nil(t, drive.PausedUntil)

	reloaded := false
	for _, record := range capture.atLeast(slog.LevelInfo) {
		if record.message == "Reloading the config (timed pause expired)" {
			reloaded = true
		}
	}
	assert.True(t, reloaded, "the expiry is reported as the reason for the reload")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.8.12
//
// Without a timed pause there is nothing to wake up for: an indefinite pause
// ends only with `resume` (an unparseable paused_until never loads, and
// config.Drive.PauseExpiry reads it as indefinite).
func TestNextPauseExpiryDelay_OnlyATimedPauseArmsTheWake(t *testing.T) {
	rd := testStandaloneMount(t, "personal:no-timed-pause@example.com", "NoTimedPause")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	clock := newTestClock()

	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	orch := NewOrchestrator(cfg)
	orch.now = clock.now

	_, ok := orch.nextPauseExpiryDelay()
	assert.False(t, ok, "a drive that is not paused arms nothing")

	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	reloadTestOrchestratorConfig(t, orch, cfgPath)
	_, ok = orch.nextPauseExpiryDelay()
	assert.False(t, ok, "an indefinite pause arms nothing")

	until := clock.now().Add(2 * time.Minute)
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", until.Format(time.RFC3339)))
	reloadTestOrchestratorConfig(t, orch, cfgPath)
	delay, ok := orch.nextPauseExpiryDelay()
	require.True(t, ok)
	assert.Equal(t, 2*time.Minute+pauseExpirySlack, delay)

	clock.advance(3 * time.Minute)
	_, ok = orch.nextPauseExpiryDelay()
	assert.False(t, ok, "a pause already past its end is for the reload to clear, not a timer")
}

func reloadTestOrchestratorConfig(t *testing.T, orch *Orchestrator, cfgPath string) {
	t.Helper()

	loaded, err := config.LoadOrDefault(cfgPath, slog.Default())
	require.NoError(t, err)
	orch.cfg.Holder.Update(loaded)
}
