package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// warningsContaining returns the WARN-and-above messages containing text.
func warningsContaining(capture *logCapture, text string) []string {
	var out []string
	for _, record := range capture.atLeast(slog.LevelWarn) {
		if strings.Contains(record.message, text) {
			out = append(out, record.message)
		}
	}
	return out
}

func blockingWatchEngine() *mockEngine {
	return &mockEngine{
		runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
}

func stopTestRunners(t *testing.T, runners map[mountID]*watchRunner) {
	t.Helper()
	for id, runner := range runners {
		runner.cancel()
		<-runner.done
		delete(runners, id)
	}
}

// A drive that keeps failing to start is retried on every reconcile pass, and
// the carried-over config failures ride along on every pass too. The user is
// told once per distinct failure, and again only after the failure changes or
// the drive has started in between.
//
// Validates: R-2.8.11
func TestReconcileWatchRunners_AnnouncesEachStartFailureOnceUntilItChanges(t *testing.T) {
	rd := testStandaloneMount(t, "personal:start-fail@example.com", "StartFail")
	setupXDGIsolation(t, rd.CanonicalID)

	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.InitialStartupResults = []MountStartupResult{{
		SelectionIndex: 0,
		Identity:       testStandaloneMountIdentity(testCanonicalID(t, "personal:bad-config@example.com")),
		DisplayName:    "BadConfig",
		Status:         MountStartupFatal,
		Err:            errors.New("validate sync config: min_free_space is not a size"),
	}}
	var warnings []StartupWarning
	cfg.StartWarning = func(warning StartupWarning) {
		warnings = append(warnings, warning)
	}
	orch := NewOrchestrator(cfg)

	startErr := errors.New("sync folder /home/u/OneDrive is missing")
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		if startErr != nil {
			return nil, startErr
		}
		return blockingWatchEngine(), nil
	}

	runners := make(map[mountID]*watchRunner)
	childWork := newParentChildWorkSnapshots()
	reconcile := func() {
		orch.reconcileWatchRunners(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{},
			runners, make(chan watchRunnerEvent, 4), childWork)
	}
	t.Cleanup(func() { stopTestRunners(t, runners) })

	for range 3 {
		reconcile()
	}
	require.Len(t, warnings, 1, "three passes over unchanged failures must warn once")
	announced := warnings[0].Summary.SkippedResults()
	require.Len(t, announced, 2)
	assert.Equal(t, "BadConfig", announced[0].DisplayName)
	assert.Equal(t, "StartFail", announced[1].DisplayName)

	startErr = errors.New("sign-in required")
	reconcile()
	require.Len(t, warnings, 2, "a changed reason is announced")
	changed := warnings[1].Summary.SkippedResults()
	require.Len(t, changed, 1)
	assert.Equal(t, "StartFail", changed[0].DisplayName)
	require.ErrorContains(t, changed[0].Err, "sign-in required")

	startErr = nil
	reconcile()
	require.Contains(t, runners, mountID(rd.CanonicalID.String()))
	assert.Len(t, warnings, 2, "a drive that starts produces no warning")

	stopTestRunners(t, runners)
	startErr = errors.New("sign-in required")
	reconcile()
	require.Len(t, warnings, 3, "a drive that started and then fails again is announced again")
	again := warnings[2].Summary.SkippedResults()
	require.Len(t, again, 1)
	assert.Equal(t, "StartFail", again[0].DisplayName)
}

// backoffHarness drives one standalone drive through runner exits and
// reconcile passes with a manual clock.
type backoffHarness struct {
	t         *testing.T
	orch      *Orchestrator
	capture   *logCapture
	clock     *testClock
	starts    atomic.Int32
	id        mountID
	runners   map[mountID]*watchRunner
	childWork *parentChildWorkSnapshots
	events    chan watchRunnerEvent
}

func newBackoffHarness(t *testing.T) *backoffHarness {
	t.Helper()

	rd := testStandaloneMount(t, "personal:backoff@example.com", "Backoff")
	setupXDGIsolation(t, rd.CanonicalID)

	capture, logger := newLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	h := &backoffHarness{
		t:         t,
		orch:      NewOrchestrator(cfg),
		capture:   capture,
		clock:     newTestClock(),
		id:        mountID(rd.CanonicalID.String()),
		runners:   make(map[mountID]*watchRunner),
		childWork: newParentChildWorkSnapshots(),
		events:    make(chan watchRunnerEvent, 4),
	}
	h.orch.now = h.clock.now
	h.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		h.starts.Add(1)
		return blockingWatchEngine(), nil
	}
	t.Cleanup(func() { stopTestRunners(t, h.runners) })
	return h
}

func (h *backoffHarness) reconcile() {
	h.orch.reconcileWatchRunners(h.t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{},
		h.runners, h.events, h.childWork)
}

// exit makes the running runner fail with err right now.
func (h *backoffHarness) exit(err error) {
	h.t.Helper()
	wr := h.runners[h.id]
	require.NotNil(h.t, wr)
	wr.cancel()
	<-wr.done
	h.orch.handleWatchRunnerEvent(h.t.Context(), watchRunnerEvent{mountID: h.id, err: err},
		syncengine.SyncBidirectional, syncengine.WatchOptions{}, h.runners, h.events, h.childWork)
}

func (h *backoffHarness) stopWarnings() []string {
	return warningsContaining(h.capture, "Stopped syncing")
}

// A runner that exits with an error is restarted after a growing delay, not
// immediately, and the user is warned once per distinct failure instead of
// every time the same failure repeats.
//
// Validates: R-2.8.11
func TestHandleWatchRunnerEvent_BacksOffAndWarnsOncePerDistinctRunnerFailure(t *testing.T) {
	h := newBackoffHarness(t)

	h.reconcile()
	require.Equal(t, int32(1), h.starts.Load())

	initialSyncFailed := errors.New("sync: initial sync failed: database is locked")
	h.exit(initialSyncFailed)
	assert.Equal(t, int32(1), h.starts.Load(), "no immediate restart")
	assert.Equal(t, []string{"Stopped syncing Backoff; retrying in 5 s"}, h.stopWarnings())

	h.clock.advance(runnerRestartBaseDelay - time.Second)
	h.reconcile()
	assert.Equal(t, int32(1), h.starts.Load(), "still inside the first backoff")
	h.clock.advance(time.Second)
	h.reconcile()
	require.Equal(t, int32(2), h.starts.Load(), "restarted once the backoff passed")

	// Same failure straight away: the wait doubles, and nothing new is said.
	h.exit(initialSyncFailed)
	assert.Len(t, h.stopWarnings(), 1, "an unchanged failure is not repeated")
	h.clock.advance(runnerRestartBaseDelay)
	h.reconcile()
	assert.Equal(t, int32(2), h.starts.Load(), "second failure waits 10 s")
	h.clock.advance(runnerRestartBaseDelay)
	h.reconcile()
	require.Equal(t, int32(3), h.starts.Load())

	// A different failure is news, and the wait keeps growing.
	h.exit(errors.New("sync: initial sync failed: disk full"))
	assert.Equal(t, []string{
		"Stopped syncing Backoff; retrying in 5 s",
		"Stopped syncing Backoff; retrying in 20 s",
	}, h.stopWarnings())
	assert.Empty(t, warningsContaining(h.capture, "watch runner"), "no jargon-only duplicate of the exit")
}

// After a runner stays up long enough, its next failure is a new incident:
// the wait starts over and the error is reported again.
//
// Validates: R-2.8.11
func TestHandleWatchRunnerEvent_HealthyRunResetsBackoff(t *testing.T) {
	h := newBackoffHarness(t)
	failure := errors.New("sync: initial sync failed: database is locked")

	h.reconcile()
	h.exit(failure)
	h.clock.advance(runnerRestartBaseDelay)
	h.reconcile()
	require.Equal(t, int32(2), h.starts.Load())

	h.clock.advance(runnerHealthyRunDuration)
	h.exit(failure)
	assert.Equal(t, []string{
		"Stopped syncing Backoff; retrying in 5 s",
		"Stopped syncing Backoff; retrying in 5 s",
	}, h.stopWarnings())
	h.clock.advance(runnerRestartBaseDelay)
	h.reconcile()
	assert.Equal(t, int32(3), h.starts.Load())
}

// The RunWatch loop itself wakes up for a backed-off restart; it does not wait
// for the next five-minute reconcile tick.
//
// Validates: R-2.8.11
func TestRunWatch_RestartsFailedRunnerWhenBackoffTimerFires(t *testing.T) {
	rd := testStandaloneMount(t, "personal:backoff-loop@example.com", "BackoffLoop")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	capture, logger := newLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	clock := newTestClock()
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

	var calls atomic.Int32
	restarted := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		call := calls.Add(1)
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				if call == 1 {
					return errors.New("sync: initial sync failed: database is locked")
				}
				close(restarted)
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
	}, 5*time.Second, 5*time.Millisecond, "restart timer was not armed")
	armedMu.Lock()
	assert.Equal(t, runnerRestartBaseDelay, armed[0])
	armedMu.Unlock()
	assert.Equal(t, int32(1), calls.Load(), "no restart before the timer fires")

	clock.advance(runnerRestartBaseDelay)
	fire <- clock.now()

	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "runner was not restarted when the backoff timer fired")
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunWatch did not stop in time")
	}

	assert.Len(t, warningsContaining(capture, "Stopped syncing BackoffLoop"), 1)
	for _, record := range capture.atLeast(slog.LevelWarn) {
		assert.NotContains(t, record.message, "context canceled", "clean shutdown must not warn")
	}
}

// An event queued by a runner the orchestrator has since stopped describes
// nothing current: it must not warn, and must not restart anything.
//
// Validates: R-2.8.11
func TestHandleWatchRunnerEvent_IgnoresEventFromAlreadyStoppedRunner(t *testing.T) {
	rd := testStandaloneMount(t, "personal:stale-event@example.com", "StaleEvent")
	setupXDGIsolation(t, rd.CanonicalID)

	capture, logger := newLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		require.Fail(t, "a stale event must not start runners")
		return nil, assert.AnError
	}

	orch.handleWatchRunnerEvent(
		t.Context(),
		watchRunnerEvent{mountID: mountID(rd.CanonicalID.String()), err: fmt.Errorf("drain: %w", context.Canceled)},
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		make(map[mountID]*watchRunner),
		make(chan watchRunnerEvent, 1),
		newParentChildWorkSnapshots(),
	)

	assert.Empty(t, capture.atLeast(slog.LevelWarn))
}

// When the orchestrator cancels a final-drain child (reload, parent snapshot
// change, shutdown), the drain's "context canceled" result is not a failure
// and must not reach the event loop, where it used to become a warning.
//
// Validates: R-2.8.11, R-2.4.8
func TestStartWatchRunner_CanceledFinalDrainSendsNoEvent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:drain-cancel@example.com", "Parent")
	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-drain-cancel", "Shortcuts/Docs")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	decisions, err := buildRuntimeWork([]StandaloneMountConfig{parent}, testParentSnapshots(&parent, child), t.TempDir())
	require.NoError(t, err)
	var childMount *mountSpec
	for i := range decisions.Mounts {
		if decisions.Mounts[i].isFinalDrainChild() {
			childMount = decisions.Mounts[i]
		}
	}
	require.NotNil(t, childMount)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	draining := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				close(draining)
				<-ctx.Done()
				return nil, fmt.Errorf("drain: %w", ctx.Err())
			},
		}, nil
	}

	events := make(chan watchRunnerEvent, 1)
	wr, err := orch.startWatchRunner(t.Context(), childMount, syncengine.SyncBidirectional, syncengine.WatchOptions{}, events)
	require.NoError(t, err)
	<-draining
	wr.cancel()
	<-wr.done

	select {
	case event := <-events:
		require.Failf(t, "unexpected event", "canceled drain sent %+v", event)
	default:
	}
}

// Another sync owning the control socket is one process-level condition. It
// is reported once, not copied onto every selected drive, and the message
// gives the way out instead of the socket path.
//
// Validates: R-2.9.1, R-2.8.2
func TestRunOnce_ControlSocketInUseIsReportedOnceForAllDrives(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:in-use-1@example.com", "InUse1")
	rd2 := testStandaloneMount(t, "personal:in-use-2@example.com", "InUse2")
	setupXDGIsolation(t, rd1.CanonicalID, rd2.CanonicalID)

	socketPath := shortControlSocketPath(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := conn.Close(); closeErr != nil {
				return
			}
		}
	}()
	defer func() {
		require.NoError(t, listener.Close())
		<-acceptDone
	}()

	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.ControlSocketPath = socketPath
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Equal(t, 1, result.Startup.SelectedCount())
	failure := result.Startup.Results[0]
	var inUse *ControlSocketInUseError
	require.ErrorAs(t, failure.Err, &inUse)
	assert.Equal(t, socketPath, inUse.Path)
	assert.NotContains(t, failure.Err.Error(), socketPath)
	assert.Contains(t, failure.Err.Error(), "another onedrive-go sync is already running")
}

// Validates: R-2.8.11
func TestRunnerRestartDelay_DoublesUpToTheCap(t *testing.T) {
	assert.Equal(t, 5*time.Second, runnerRestartDelay(1))
	assert.Equal(t, 10*time.Second, runnerRestartDelay(2))
	assert.Equal(t, 20*time.Second, runnerRestartDelay(3))
	assert.Equal(t, runnerRestartMaxDelay, runnerRestartDelay(7))
	assert.Equal(t, runnerRestartMaxDelay, runnerRestartDelay(100))
}

func TestHumanDelay_UsesHumanUnits(t *testing.T) {
	assert.Equal(t, "5 s", humanDelay(5*time.Second))
	assert.Equal(t, "2 min", humanDelay(2*time.Minute))
	assert.Equal(t, "2 min 30 s", humanDelay(150*time.Second))
}

// Validates: R-2.8.11
func TestWarnOnChange_RepeatsOfTheSameFailureStayBelowWarn(t *testing.T) {
	capture, logger := newLogCapture()
	orch := &Orchestrator{logger: logger, cfg: &OrchestratorConfig{}}

	for range 3 {
		orch.warnOnChange("k", "Couldn't update shortcuts", errors.New("bad command"))
	}
	assert.Len(t, capture.atLeast(slog.LevelWarn), 1)

	orch.warnOnChange("k", "Couldn't update shortcuts", errors.New("other reason"))
	assert.Len(t, capture.atLeast(slog.LevelWarn), 2)

	orch.clearWarnOnChange("k")
	orch.warnOnChange("k", "Couldn't update shortcuts", errors.New("other reason"))
	assert.Len(t, capture.atLeast(slog.LevelWarn), 3, "a failure that cleared and came back is reported again")
}

// A panic in one mount's watch loop is that mount's failure: it is reported
// through the runner event path, backed off and restarted like any other
// runner error, and its siblings never notice. Before the runner goroutine
// had a recover, the panic unwound the process and every other mount with it.
//
// Validates: R-6.7.1
func TestRunWatch_RunnerPanicFailsOnlyThatMountAndRestartsIt(t *testing.T) {
	panicky := testStandaloneMount(t, "personal:panicky@example.com", "Panicky")
	sibling := testStandaloneMount(t, "personal:sibling@example.com", "Sibling")
	cfgPath := writeTestConfig(t, panicky.CanonicalID, sibling.CanonicalID)
	capture, logger := newLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, panicky, sibling)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	clock := newTestClock()
	orch.now = clock.now
	orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

	fire := make(chan time.Time, 1)
	armed := make(chan time.Duration, 4)
	orch.restartTimer = func(delay time.Duration) (<-chan time.Time, func()) {
		armed <- delay
		return fire, func() {}
	}

	engines := newPanicIsolationEngines(sibling.CanonicalID)
	orch.engineFactory = engines.factory

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case <-engines.siblingStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "sibling mount did not start")
	}

	// The panic reaches the loop as a runner failure: a restart is armed with
	// the first backoff delay, and the drive is reported stopped once.
	select {
	case delay := <-armed:
		assert.Equal(t, runnerRestartBaseDelay, delay)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the panicking mount's restart was never armed")
	}
	require.Len(t, warningsContaining(capture, "Stopped syncing Panicky"), 1)

	errorsLogged := capture.atLeast(slog.LevelError)
	require.Len(t, errorsLogged, 1, "the panic is logged once, at Error")
	assert.Equal(t, "Panicky", errorsLogged[0].attrs["drive"])
	assert.Equal(t, "watch loop exploded", errorsLogged[0].attrs["panic"])
	assert.Contains(t, errorsLogged[0].attrs["stack"], "goroutine")

	select {
	case <-engines.siblingStopped:
		require.FailNow(t, "a panic in one mount must not stop its sibling")
	case err := <-errCh:
		require.FailNow(t, "RunWatch must survive a mount panic", "returned %v", err)
	default:
	}

	clock.advance(runnerRestartBaseDelay)
	fire <- clock.now()
	select {
	case <-engines.panickyRestarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the panicking mount was not restarted when its backoff fired")
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunWatch did not stop in time")
	}
	assert.Equal(t, int32(2), engines.panickyStarts.Load())
}

// panicIsolationEngines builds the two engines of the panic-isolation test:
// one whose first watch run panics and whose second run blocks, and a sibling
// that blocks until canceled and reports when it starts and stops.
type panicIsolationEngines struct {
	siblingCanonicalID driveid.CanonicalID
	panickyStarts      atomic.Int32
	panickyRestarted   chan struct{}
	siblingStarted     chan struct{}
	siblingStopped     chan struct{}
}

func newPanicIsolationEngines(siblingCanonicalID driveid.CanonicalID) *panicIsolationEngines {
	return &panicIsolationEngines{
		siblingCanonicalID: siblingCanonicalID,
		panickyRestarted:   make(chan struct{}),
		siblingStarted:     make(chan struct{}),
		siblingStopped:     make(chan struct{}),
	}
}

func (e *panicIsolationEngines) factory(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
	if req.Mount.parentCanonicalID() == e.siblingCanonicalID {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				close(e.siblingStarted)
				<-ctx.Done()
				close(e.siblingStopped)
				return ctx.Err()
			},
		}, nil
	}
	start := e.panickyStarts.Add(1)
	return &mockEngine{
		runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
			if start == 1 {
				panic("watch loop exploded")
			}
			close(e.panickyRestarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}, nil
}
