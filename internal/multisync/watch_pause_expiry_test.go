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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
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
	w := newTimedPauseWatch(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	require.Eventually(t, func() bool {
		return len(w.armedDelays()) > 0
	}, 5*time.Second, 5*time.Millisecond, "the pause-expiry wake-up was not armed")
	assert.Equal(t, w.wait, w.armedDelays()[0], "armed for the pause's end")
	select {
	case <-w.started:
		require.FailNow(t, "the drive must not run while its pause holds")
	default:
	}

	w.clock.advance(w.wait)
	w.fire <- w.clock.now()

	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the drive did not start when its timed pause expired")
	}

	loaded, err := config.LoadOrDefault(w.cfgPath, w.logger)
	require.NoError(t, err)
	drive := loaded.Drives[w.mount.CanonicalID]
	assert.Nil(t, drive.Paused, "the expired pause is cleared from the config")
	assert.Nil(t, drive.PausedUntil)
	assert.True(t, w.logged("Reloading the config (timed pause expired)"),
		"the expiry is reported as the reason for the reload")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunWatch did not stop in time")
	}
}

// timedPauseWatch is an orchestrator whose one drive is paused until an hour
// from now on the wall clock, running on a test clock the test moves past
// that end, with the restart timer and the engine factory stubbed.
type timedPauseWatch struct {
	orch    *Orchestrator
	mount   StandaloneMountConfig
	cfgPath string
	clock   *testClock
	wait    time.Duration
	fire    chan time.Time
	started chan struct{}
	logger  *slog.Logger
	capture *synctest.LogCapture
	// armedMu guards armed: the loop arms the timer from its own goroutine.
	armedMu gosync.Mutex
	armed   []time.Duration
}

func newTimedPauseWatch(t *testing.T) *timedPauseWatch {
	t.Helper()

	rd := testStandaloneMount(t, "personal:timed-pause@example.com", "TimedPause")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	// The pause ends in the wall clock's future (config loading reads an
	// already-past paused_until as expired), while the orchestrator runs on
	// the test clock.
	clock := newTestClock()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", until.Format(time.RFC3339)))

	capture, logger := synctest.NewLogCapture()
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

	w := &timedPauseWatch{
		orch:    NewOrchestrator(cfg),
		mount:   rd,
		cfgPath: cfgPath,
		clock:   clock,
		wait:    until.Sub(clock.now()) + pauseExpirySlack,
		fire:    make(chan time.Time, 1),
		started: make(chan struct{}),
		logger:  logger,
		capture: capture,
	}
	// The production holder carries the loaded file; the test helper's holds
	// defaults, so give it the file with the pause in it.
	reloadTestOrchestratorConfig(t, w.orch, cfgPath)
	w.orch.now = clock.now
	w.orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	w.orch.restartTimer = func(delay time.Duration) (<-chan time.Time, func()) {
		w.armedMu.Lock()
		w.armed = append(w.armed, delay)
		w.armedMu.Unlock()
		return w.fire, func() {}
	}
	var startOnce gosync.Once
	w.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				startOnce.Do(func() { close(w.started) })
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	return w
}

func (w *timedPauseWatch) armedDelays() []time.Duration {
	w.armedMu.Lock()
	defer w.armedMu.Unlock()

	return append([]time.Duration(nil), w.armed...)
}

func (w *timedPauseWatch) logged(message string) bool {
	for _, record := range w.capture.AtLeast(slog.LevelInfo) {
		if record.Message == message {
			return true
		}
	}

	return false
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
