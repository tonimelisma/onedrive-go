package multisync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

func TestLoopHeartbeat_DeclaredStepExtendsTheLimit(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	h := newLoopHeartbeat(clock.now)

	clock.advance(loopSilenceLimit)
	ok, _, _ := h.alive()
	assert.True(t, ok, "silent exactly the limit is still alive")
	clock.advance(time.Second)
	ok, _, silent := h.alive()
	assert.False(t, ok)
	assert.Equal(t, loopSilenceLimit+time.Second, silent)

	h.beatFor("stopping a drive", 3*loopSilenceLimit)
	clock.advance(2 * loopSilenceLimit)
	ok, step, _ := h.alive()
	assert.True(t, ok, "a declared step holds the loop up to its own bound")
	assert.Equal(t, "stopping a drive", step)
	clock.advance(loopSilenceLimit + time.Second)
	ok, _, _ = h.alive()
	assert.False(t, ok, "past the declared bound the loop counts as hung")

	h.beat()
	ok, step, silent = h.alive()
	assert.True(t, ok)
	assert.Empty(t, step)
	assert.Zero(t, silent)

	h.beatFor("short", time.Millisecond)
	clock.advance(loopSilenceLimit)
	ok, _, _ = h.alive()
	assert.True(t, ok, "a declared step never shortens the ordinary limit")
}

func TestRunWatch_BeatsOnEveryIterationAndWhileIdle(t *testing.T) {
	rd := testStandaloneMount(t, "personal:heartbeat@example.com", "Heartbeat")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	clock := newTestClock()
	orch.now = clock.now
	orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	beats := make(chan time.Time)
	var interval time.Duration
	orch.heartbeatTicks = func(d time.Duration) (<-chan time.Time, func()) {
		interval = d
		return beats, func() {}
	}
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncdomain.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	cancel, done := runWatchInBackground(t, orch)
	defer stopWatch(t, cancel, done)

	// The loop is idle; each tick it takes is a beat at the top of the next
	// iteration, so a clock advanced past the silence limit between ticks
	// still reads alive once the tick has been taken.
	for range 3 {
		clock.advance(loopSilenceLimit + time.Second)
		select {
		case beats <- clock.now():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the watch loop did not take its heartbeat tick")
		}
		// The send hands the tick over; the next send can happen only
		// after the loop has gone round and beaten, which the next
		// iteration of this loop waits for.
	}
	select {
	case beats <- clock.now():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the watch loop did not take its heartbeat tick")
	}
	require.Eventually(t, func() bool {
		ok, step, _ := orch.LoopAlive()
		return ok && step == ""
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, loopHeartbeatInterval, interval)

	clock.advance(loopSilenceLimit + time.Second)
	ok, _, _ := orch.LoopAlive()
	assert.False(t, ok, "a loop that takes no tick goes silent")
}

// Validates: R-6.6.36
func TestLoopHeartbeat_AnInnerStepEndsBackInTheOuterStep(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	h := newLoopHeartbeat(clock.now)

	h.beatFor("reloading the config", 3*loopSilenceLimit)
	endInner := h.beatFor("stopping a drive", 2*loopSilenceLimit)
	clock.advance(2 * loopSilenceLimit)
	ok, step, _ := h.alive()
	assert.True(t, ok)
	assert.Equal(t, "stopping a drive", step, "the innermost step is the one named")

	endInner()
	ok, step, silent := h.alive()
	assert.True(t, ok, "ending the inner step is progress")
	assert.Equal(t, "reloading the config", step, "the outer step is restored")
	assert.Zero(t, silent)

	clock.advance(loopSilenceLimit)
	ok, _, _ = h.alive()
	assert.True(t, ok, "the outer step keeps its own deadline")
	clock.advance(time.Second)
	ok, _, _ = h.alive()
	assert.False(t, ok, "past the outer step's deadline the loop counts as hung")

	// An inner step that outlasted the outer one's deadline leaves the outer
	// step the ordinary limit for the rest of its work, not an overdue
	// deadline.
	h.beat()
	h.beatFor("reloading the config", loopSilenceLimit)
	endInner = h.beatFor("starting a drive", 4*loopSilenceLimit)
	clock.advance(3 * loopSilenceLimit)
	endInner()
	clock.advance(loopSilenceLimit)
	ok, step, _ = h.alive()
	assert.True(t, ok)
	assert.Equal(t, "reloading the config", step)

	// A step whose end comes after a later beat changes nothing.
	endLate := h.beatFor("stopping a drive", loopSilenceLimit)
	h.beat()
	endLate()
	ok, step, _ = h.alive()
	assert.True(t, ok)
	assert.Empty(t, step)
}

// Validates: R-6.6.36
func TestLoopHeartbeat_AFinishedLoopIsNeverHung(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	h := newLoopHeartbeat(clock.now)
	h.finish()
	clock.advance(10 * watchLoopStepLimit)

	ok, step, silent := h.alive()
	assert.True(t, ok, "once RunWatch returned there is no loop to judge")
	assert.Empty(t, step)
	assert.Zero(t, silent)
}

// blockingLogHandler holds the first record whose message contains match
// until release is closed: a log line the console or the journal does not
// take, which is a call the watch loop blocks in.
type blockingLogHandler struct {
	match   string
	reached chan struct{}
	release chan struct{}
	once    *gosync.Once
}

func newBlockingLogHandler(match string) *blockingLogHandler {
	return &blockingLogHandler{
		match: match, reached: make(chan struct{}), release: make(chan struct{}), once: &gosync.Once{},
	}
}

func (h *blockingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (h *blockingLogHandler) Handle(_ context.Context, record slog.Record) error {
	if strings.Contains(record.Message, h.match) {
		h.once.Do(func() {
			close(h.reached)
			<-h.release
		})
	}

	return nil
}

func (h *blockingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *blockingLogHandler) WithGroup(string) slog.Handler { return h }

// requireDeclaredStepWhileBlocked is called while the loop is blocked in one
// of its cases: past the ordinary silence limit the loop still reads alive in
// step, and past the step's own limit it reads hung, so the watchdog stops
// vouching for it.
func requireDeclaredStepWhileBlocked(t *testing.T, orch *Orchestrator, clock *testClock, step string) {
	t.Helper()

	clock.advance(loopSilenceLimit + time.Second)
	ok, gotStep, _ := orch.LoopAlive()
	assert.True(t, ok, "a blocking call inside a declared step is not a hang")
	assert.Equal(t, step, gotStep)

	clock.advance(watchLoopStepLimit)
	ok, _, _ = orch.LoopAlive()
	assert.False(t, ok, "past the step's limit the loop counts as hung")
}

// heartbeatWatch is a watch on an injected clock with one drive, no
// reconcile ticks and no heartbeat ticks unless a test sends them.
func heartbeatWatch(t *testing.T, logger *slog.Logger, runWatch func(ctx context.Context) error) (*Orchestrator, *OrchestratorConfig, *testClock) {
	t.Helper()

	rd := testStandaloneMount(t, "personal:heartbeat-step@example.com", "HeartbeatStep")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	clock := newTestClock()
	orch.now = clock.now
	orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	orch.heartbeatTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncdomain.SyncMode, _ syncengine.WatchOptions) error {
				return runWatch(ctx)
			},
		}, nil
	}

	return orch, cfg, clock
}

func waitUntilDrivesRun(t *testing.T, started <-chan struct{}) {
	t.Helper()

	receiveWithin(t, started, "the drive to start watching")
}

// Validates: R-6.6.36
//
// Each case of the watch loop that can block declares its step before it
// does, so a slow reload, a slow drive event and a slow pass over the drive
// set read as progress up to their limit, and as a hang past it.
func TestRunWatch_ABlockingCaseIsADeclaredStep(t *testing.T) {
	t.Run("a reload a client asked for", func(t *testing.T) {
		started := make(chan struct{}, 1)
		orch, cfg, clock := heartbeatWatch(t, slog.New(slog.DiscardHandler), func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
		compiling := make(chan struct{})
		release := make(chan struct{})
		compile := cfg.ReloadStandaloneMounts
		cfg.ReloadStandaloneMounts = func(loaded *config.Config) (StandaloneMountSelection, error) {
			close(compiling)
			<-release
			return compile(loaded)
		}

		cancel, done := runWatchInBackground(t, orch)
		waitUntilDrivesRun(t, started)
		answered := make(chan int, 1)
		go func() { answered <- postReloadWithoutAssertions(t.Context(), cfg.ControlSocketPath) }()
		receiveWithin(t, compiling, "the reload to compile the config")

		requireDeclaredStepWhileBlocked(t, orch, clock, reloadStep)

		close(release)
		assert.Equal(t, http.StatusOK, receiveWithin(t, answered, "the reload's answer"))
		stopWatch(t, cancel, done)
	})

	t.Run("a pass over the drive set", func(t *testing.T) {
		started := make(chan struct{}, 1)
		blocked := newBlockingLogHandler("drive set unchanged")
		orch, _, clock := heartbeatWatch(t, slog.New(blocked), func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
		ticks := make(chan time.Time, 1)
		orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }

		cancel, done := runWatchInBackground(t, orch)
		waitUntilDrivesRun(t, started)
		ticks <- clock.now()
		receiveWithin(t, blocked.reached, "the pass to log its outcome")

		requireDeclaredStepWhileBlocked(t, orch, clock, "updating the drive set")

		close(blocked.release)
		stopWatch(t, cancel, done)
	})

	t.Run("a drive's event", func(t *testing.T) {
		blocked := newBlockingLogHandler("Stopped syncing")
		orch, _, clock := heartbeatWatch(t, slog.New(blocked), func(context.Context) error {
			return errors.New("sync: initial sync failed: database is locked")
		})
		// The failed drive's restart waits out its backoff on a timer that
		// never fires here.
		orch.restartTimer = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

		cancel, done := runWatchInBackground(t, orch)
		receiveWithin(t, blocked.reached, "the loop to report the drive's exit")

		requireDeclaredStepWhileBlocked(t, orch, clock, "handling a drive's event")

		close(blocked.release)
		stopWatch(t, cancel, done)
	})
}

// postReloadWithoutAssertions asks the watch at socketPath for a reload and
// returns the status it answered with, or zero when the request failed. It
// makes no assertion, so a test can send it from a goroutine of its own.
func postReloadWithoutAssertions(ctx context.Context, socketPath string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, synccontrol.HTTPBaseURL+synccontrol.PathReload, http.NoBody)
	if err != nil {
		return 0
	}
	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := controlTestClient(socketPath).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	return resp.StatusCode
}
