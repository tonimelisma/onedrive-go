package multisync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// testWait bounds how long a test waits for something it expects to happen
// at once.
const testWait = 5 * time.Second

// firedTimer is a retire timer that has already run out: the bound it arms
// is over the moment it is armed.
func firedTimer() (<-chan time.Time, func()) {
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	return fired, func() {}
}

// handMadeRunner is a runner without a goroutine: its done is closed when
// exited is true and never closed otherwise, as for a sync that ignores its
// cancellation.
func handMadeRunner(mount *mountSpec, exited bool) *watchRunner {
	done := make(chan struct{})
	if exited {
		close(done)
	}
	return &watchRunner{mount: mount, engine: &mockEngine{}, cancel: func() {}, done: done, generation: 1}
}

// handleEventWithin runs handleWatchRunnerEvent on its own goroutine and
// fails the test when it has not returned in time; before runner events
// carried a generation, an event from a replaced runner blocked on the
// replacement's done channel.
func handleEventWithin(
	t *testing.T,
	orch *Orchestrator,
	event watchRunnerEvent,
	runners map[mountID]*watchRunner,
) {
	t.Helper()

	handled := make(chan error, 1)
	ctx := t.Context()
	go func() {
		handled <- orch.handleWatchRunnerEvent(ctx, event, syncengine.SyncBidirectional, syncengine.WatchOptions{},
			runners, make(chan watchRunnerEvent, 1), newParentChildWorkSnapshots())
	}()

	select {
	case err := <-handled:
		require.NoError(t, err)
	case <-time.After(testWait):
		require.FailNow(t, "the event of a replaced runner was applied to its replacement")
	}
}

// finalDrainMount builds the runtime spec of a shortcut child of parent that
// is in its final drain.
func finalDrainMount(t *testing.T, parent *StandaloneMountConfig, bindingID string) *mountSpec {
	t.Helper()

	child := finalDrainChildRecord(t, mountID(parent.CanonicalID.String()), bindingID, "Shortcuts/"+bindingID)
	decisions, err := buildRuntimeWork([]StandaloneMountConfig{*parent}, testParentSnapshots(parent, child), t.TempDir())
	require.NoError(t, err)
	for i := range decisions.Mounts {
		if decisions.Mounts[i].isFinalDrainChild() {
			return decisions.Mounts[i]
		}
	}
	require.FailNow(t, "no final-drain child in the runtime work")
	return nil
}

// A runner fails on its own and queues its exit; before the loop reads it, a
// reload replaces the runner under the same mount ID. The queued event
// describes the old runner and must not touch the new one.
//
// Validates: R-6.3.9
func TestHandleWatchRunnerEvent_EventFromReplacedRunnerIsIgnored(t *testing.T) {
	rd := testStandaloneMount(t, "personal:replaced-runner@example.com", "Replaced")
	setupXDGIsolation(t, rd.CanonicalID)

	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return nil, errors.New("a stale event must not start runners")
	}

	mounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	id := mounts[0].id()
	replacement := handMadeRunner(mounts[0], false)
	replacement.generation = 2
	runners := map[mountID]*watchRunner{id: replacement}

	handleEventWithin(t, orch, watchRunnerEvent{
		mountID:    id,
		generation: 1,
		err:        errors.New("sync folder is missing"),
	}, runners)

	assert.Same(t, replacement, runners[id], "the replacement keeps running")
	assert.Empty(t, capture.AtLeast(slog.LevelWarn), "the old runner's exit is not reported against its replacement")
}

// The same race for a shortcut child that a parent snapshot turned into a
// final drain: the normal child's queued failure must not be treated as the
// final drain's result.
//
// Validates: R-6.3.9, R-2.4.8
func TestHandleWatchRunnerEvent_EventFromReplacedFinalDrainChildIsIgnored(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:replaced-drain@example.com", "Parent")
	drainMount := finalDrainMount(t, &parent, "binding-replaced")

	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return nil, errors.New("a stale event must not start runners")
	}

	drain := handMadeRunner(drainMount, false)
	drain.generation = 2
	runners := map[mountID]*watchRunner{drainMount.id(): drain}

	handleEventWithin(t, orch, watchRunnerEvent{
		mountID:    drainMount.id(),
		generation: 1,
		err:        errors.New("sync folder is missing"),
	}, runners)

	assert.Same(t, drain, runners[drainMount.id()], "the final drain keeps running")
	assert.Empty(t, warningsContaining(capture, "Couldn't finish removing shortcut"))
}

// A shortcut removed on OneDrive while watching is republished by its parent
// with only the run mode changed. The normal runner must make way for a
// final drain; when the mode was not part of runner equivalence, the normal
// runner kept running and the drain never started.
//
// Validates: R-6.3.9, R-2.4.8
func TestApplyWatchMountSetForParent_NormalToFinalDrainRestartsChild(t *testing.T) {
	parent := testStandaloneMount(t, "personal:normal-to-drain@example.com", "NormalToDrain")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	parentID := mountID(parent.CanonicalID.String())

	normal := testChildRecord(t, parentID, "binding-drain-later", "Shortcuts/DrainLater")
	draining := normal
	draining.Mode = syncengine.ShortcutChildRunModeFinalDrain

	parentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	oldDecisions, err := buildRuntimeWorkForParents(parentMounts, testParentSnapshots(&parent, normal), cfg.DataDir, nil)
	require.NoError(t, err)
	nextParentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	nextDecisions, err := buildRuntimeWorkForParents(nextParentMounts, testParentSnapshots(&parent, draining), cfg.DataDir, nil)
	require.NoError(t, err)

	var normalMount *mountSpec
	for i := range oldDecisions.Mounts {
		if oldDecisions.Mounts[i].projectionKind() == MountProjectionChild {
			normalMount = oldDecisions.Mounts[i]
		}
	}
	require.NotNil(t, normalMount)
	require.False(t, normalMount.isFinalDrainChild())

	normalDone := make(chan struct{})
	stops := 0
	runners := map[mountID]*watchRunner{
		normalMount.id(): {
			mount:  normalMount,
			engine: &mockEngine{},
			cancel: func() {
				if stops == 0 {
					close(normalDone)
				}
				stops++
			},
			done:       normalDone,
			generation: 1,
		},
	}
	drained := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
				close(drained)
				return &syncengine.Report{}, nil
			},
		}, nil
	}

	stopped, started, _, err := orch.applyWatchMountSetForParent(
		t.Context(),
		runners,
		parentID,
		nextDecisions,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		make(chan watchRunnerEvent, 4),
		newParentChildWorkSnapshots(),
	)

	require.NoError(t, err)
	assert.Equal(t, 1, stopped, "the normal runner is stopped")
	assert.Equal(t, 1, started, "a final-drain runner replaces it")
	assert.Equal(t, 1, stops)
	select {
	case <-drained:
	case <-time.After(testWait):
		require.FailNow(t, "the final drain did not run")
	}
	stopTestRunners(t, runners)
}

// A runner that ignores its cancellation fails closed: the bounded wait gives
// up, the runner stays in the runner set (so nothing starts in its place),
// its engine is not closed under it, and one Error line carries every
// goroutine's stack, the stuck one's included.
//
// Validates: R-6.3.9
func TestRetireWatchRunner_TimeoutFailsClosed(t *testing.T) {
	h := newBackoffHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	engine := &mockEngine{
		runWatchFn: func(context.Context, syncengine.SyncMode, syncengine.WatchOptions) error {
			<-release
			return nil
		},
	}
	h.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		h.starts.Add(1)
		return engine, nil
	}
	var armed []time.Duration
	h.orch.retireTimer = func(bound time.Duration) (<-chan time.Time, func()) {
		armed = append(armed, bound)
		return firedTimer()
	}

	h.reconcile()
	wr := h.runners[h.id]
	require.NotNil(t, wr)

	err := h.orch.retireWatchRunner(t.Context(), h.runners, h.id)

	require.ErrorIs(t, err, ErrSyncStuck)
	require.ErrorContains(t, err, "Backoff")
	assert.Equal(t, []time.Duration{watchRunnerStopTimeout}, armed, "the stop wait runs the full bound")
	assert.Same(t, wr, h.runners[h.id], "a stuck runner stays in the runner set")
	assert.False(t, engine.closed, "a stuck runner's engine is not closed under it")
	errorLogs := h.capture.AtLeast(slog.LevelError)
	require.Len(t, errorLogs, 1)
	assert.Contains(t, errorLogs[0].Message, "Backoff")
	assert.Contains(t, errorLogs[0].Attrs["stack"], "startWatchRunner", "the dump shows the stuck runner's goroutine")

	require.ErrorIs(t, h.orch.retireWatchRunner(t.Context(), h.runners, h.id), ErrSyncStuck,
		"a stuck runner stays stuck")
	assert.Len(t, armed, 1, "and is not waited for again")
	h.clock.advance(runnerRestartMaxDelay)
	h.reconcile()
	assert.Equal(t, int32(1), h.starts.Load(), "nothing starts in place of a stuck runner")
}

// Engine.Close does not honor its context (the store's checkpoint runs
// detached), so the close bound is on the wait. A close that runs over fails
// closed the same way a stop does.
//
// Validates: R-6.3.9
func TestRetireWatchRunner_EngineCloseTimeoutFailsClosed(t *testing.T) {
	h := newBackoffHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		h.starts.Add(1)
		return &mockEngine{
			closeFn: func(context.Context) error {
				<-release
				return nil
			},
		}, nil
	}
	never := make(chan time.Time)
	h.orch.retireTimer = func(bound time.Duration) (<-chan time.Time, func()) {
		if bound == engineCloseTimeout {
			return firedTimer()
		}
		return never, func() {}
	}

	h.reconcile()
	wr := h.runners[h.id]
	require.NotNil(t, wr)

	err := h.orch.retireWatchRunner(t.Context(), h.runners, h.id)

	require.ErrorIs(t, err, ErrSyncStuck)
	require.ErrorContains(t, err, "sync state did not close")
	assert.Same(t, wr, h.runners[h.id])
	assert.Len(t, h.capture.AtLeast(slog.LevelError), 1)
}

// stuckStopFixture is a drive with one shortcut child, on an orchestrator
// whose stop bound is over as soon as it is armed and which counts starts.
// The close bound never runs out: the fakes close at once.
type stuckStopFixture struct {
	orch      *Orchestrator
	capture   *synctest.LogCapture
	parent    *mountSpec
	child     *mountSpec
	childWork *parentChildWorkSnapshots
	starts    int
	stopped   int
}

func newStuckStopFixture(t *testing.T, paused bool) *stuckStopFixture {
	t.Helper()

	parent := testStandaloneMount(t, "personal:stuck-paths@example.com", "StuckPaths")
	setupXDGIsolation(t, parent.CanonicalID)
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	f := &stuckStopFixture{orch: NewOrchestrator(cfg), capture: capture, childWork: newParentChildWorkSnapshots()}
	never := make(chan time.Time)
	f.orch.retireTimer = func(bound time.Duration) (<-chan time.Time, func()) {
		if bound == engineCloseTimeout {
			return never, func() {}
		}
		return firedTimer()
	}
	f.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		f.starts++
		return blockingWatchEngine(), nil
	}

	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-stuck-paths", "Shortcuts/StuckPaths")
	seedShortcutChildRunCommand(f.childWork, &parent, &child)
	decisions, err := f.orch.buildRuntimeWorkSet(t.Context(), cfg.StandaloneMounts, nil, f.childWork)
	require.NoError(t, err)
	for _, mount := range decisions.Mounts {
		switch mount.projectionKind() {
		case MountProjectionStandalone:
			f.parent = mount
		case MountProjectionChild:
			f.child = mount
		}
	}
	require.NotNil(t, f.parent)
	require.NotNil(t, f.child)

	if paused {
		parent.Paused = true
		f.orch.cfg.StandaloneMounts = []StandaloneMountConfig{parent}
	}

	return f
}

// applySet applies decisions to runners as a reload or reconcile pass does,
// and adds the runners the pass reports stopped to f.stopped.
func (f *stuckStopFixture) applySet(ctx context.Context, runners map[mountID]*watchRunner, decisions *runtimeWorkSet) error {
	stopped, _, _, err := f.orch.applyWatchMountSet(ctx, runners, decisions, syncengine.SyncBidirectional,
		syncengine.WatchOptions{}, make(chan watchRunnerEvent, 4), f.childWork)
	f.stopped += stopped
	return err
}

// applyConfiguredSet applies the set the configured drives produce now.
func (f *stuckStopFixture) applyConfiguredSet(ctx context.Context, runners map[mountID]*watchRunner) error {
	decisions, err := f.orch.buildRuntimeWorkSet(ctx, f.orch.cfg.StandaloneMounts, nil, f.childWork)
	if err != nil {
		return err
	}
	return f.applySet(ctx, runners, decisions)
}

// applyEmptyParentSnapshot applies a snapshot of the fixture drive that
// lists no shortcut, as its runner publishes once the shortcut is gone.
func (f *stuckStopFixture) applyEmptyParentSnapshot(ctx context.Context, runners map[mountID]*watchRunner) error {
	stopped, _, _, err := f.orch.applyWatchMountSetForParent(ctx, runners, f.parent.id(), &runtimeWorkSet{},
		syncengine.SyncBidirectional, syncengine.WatchOptions{}, make(chan watchRunnerEvent, 4), f.childWork)
	f.stopped += stopped
	return err
}

func (f *stuckStopFixture) handle(ctx context.Context, runners map[mountID]*watchRunner, event watchRunnerEvent) error {
	return f.orch.handleWatchRunnerEvent(ctx, event, syncengine.SyncBidirectional, syncengine.WatchOptions{},
		runners, make(chan watchRunnerEvent, 4), f.childWork)
}

// Every path that stops a watch runner goes through the bounded stop, and a
// runner that does not stop fails closed on each of them: the path ends with
// ErrSyncStuck instead of waiting forever, the runner stays in the
// runner set with its engine open, and nothing is started. Before the bound,
// each of these waited on the runner's done channel with no limit.
//
// Validates: R-6.3.9
func TestWatchRunnerStopPaths_ARunnerThatDoesNotStopFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		paused bool
		// runners builds the runner set; stuck is the runner that ignores
		// its cancellation.
		runners func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner)
		stop    func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error
	}{
		{
			name: "drive removed by a reload",
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.parent, false)
				return map[mountID]*watchRunner{f.parent.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.applySet(ctx, runners, &runtimeWorkSet{})
			},
		},
		{
			name:   "drive paused",
			paused: true,
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.parent, false)
				return map[mountID]*watchRunner{f.parent.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.applyConfiguredSet(ctx, runners)
			},
		},
		{
			name: "shortcut retired by its drive's new snapshot",
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.child, false)
				return map[mountID]*watchRunner{f.parent.id(): handMadeRunner(f.parent, false), f.child.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.applyEmptyParentSnapshot(ctx, runners)
			},
		},
		{
			name: "shortcut stopped because its drive exited",
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.child, false)
				return map[mountID]*watchRunner{f.parent.id(): handMadeRunner(f.parent, true), f.child.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.handle(ctx, runners, watchRunnerEvent{mountID: f.parent.id(), generation: 1, err: errors.New("drive failed")})
			},
		},
		{
			name: "runner whose exit hangs in its cleanup",
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.parent, false)
				return map[mountID]*watchRunner{f.parent.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.handle(ctx, runners, watchRunnerEvent{mountID: f.parent.id(), generation: 1, err: errors.New("drive failed")})
			},
		},
		{
			name: "the watch ending",
			runners: func(f *stuckStopFixture) (map[mountID]*watchRunner, *watchRunner) {
				stuck := handMadeRunner(f.parent, false)
				return map[mountID]*watchRunner{f.parent.id(): stuck}, stuck
			},
			stop: func(ctx context.Context, f *stuckStopFixture, runners map[mountID]*watchRunner) error {
				return f.orch.retireAllWatchRunners(ctx, runners)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStuckStopFixture(t, tt.paused)
			runners, stuck := tt.runners(f)

			err := tt.stop(t.Context(), f, runners)

			require.ErrorIs(t, err, ErrSyncStuck)
			assert.Same(t, stuck, runners[stuck.mount.id()], "the stuck runner stays in the runner set")
			require.ErrorIs(t, stuck.stuckErr, ErrSyncStuck)
			engine, ok := stuck.engine.(*mockEngine)
			require.True(t, ok)
			assert.False(t, engine.closed, "its engine is not closed under it")
			assert.Zero(t, f.starts, "nothing is started")
			assert.Zero(t, f.stopped, "a stuck runner is not reported stopped")
			assert.Len(t, f.capture.AtLeast(slog.LevelError), 1)
		})
	}
}

// When the watch ends, every runner is canceled before any is waited for, so
// two drives that both ignore the stop cost one bound, not one each: a
// shutdown stays inside the service manager's stop timeout.
//
// Validates: R-6.3.9
func TestRetireAllWatchRunners_StuckDrivesShareOneStopBound(t *testing.T) {
	f := newStuckStopFixture(t, false)
	clock := newTestClock()
	f.orch.now = clock.now
	var events []string
	f.orch.retireTimer = func(bound time.Duration) (<-chan time.Time, func()) {
		events = append(events, "wait "+bound.String())
		clock.advance(bound)
		return firedTimer()
	}
	parent := handMadeRunner(f.parent, false)
	parent.cancel = func() { events = append(events, "cancel drive") }
	child := handMadeRunner(f.child, false)
	child.cancel = func() { events = append(events, "cancel shortcut") }
	runners := map[mountID]*watchRunner{f.parent.id(): parent, f.child.id(): child}

	err := f.orch.retireAllWatchRunners(t.Context(), runners)

	require.ErrorIs(t, err, ErrSyncStuck)
	assert.Equal(t, []string{"cancel shortcut", "cancel drive", "wait " + watchRunnerStopTimeout.String()}, events)
	assert.Len(t, runners, 2)
	assert.Len(t, f.capture.AtLeast(slog.LevelError), 2, "each stuck drive is logged")
}

// stuckWatch is a running watch over one drive whose sync ignores its
// cancellation, on an orchestrator whose retire bound is over at once.
type stuckWatch struct {
	cfg     *OrchestratorConfig
	cfgPath string
	capture *synctest.LogCapture
	cancel  context.CancelFunc
	errCh   chan error
	starts  atomic.Int32
}

func startStuckWatch(t *testing.T, cidStr, name string) *stuckWatch {
	t.Helper()

	rd := testStandaloneMount(t, cidStr, name)
	setupXDGIsolation(t, rd.CanonicalID)
	w := &stuckWatch{cfgPath: writeTestConfig(t, rd.CanonicalID), errCh: make(chan error, 1)}
	var logger *slog.Logger
	w.capture, logger = synctest.NewLogCapture()
	w.cfg = testOrchestratorConfigWithPath(t, w.cfgPath, rd)
	w.cfg.ControlSocketPath = shortControlSocketPath(t)
	w.cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	w.cfg.Logger = logger
	orch := NewOrchestrator(w.cfg)
	orch.retireTimer = func(time.Duration) (<-chan time.Time, func()) { return firedTimer() }

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		w.starts.Add(1)
		return &mockEngine{
			runWatchFn: func(context.Context, syncengine.SyncMode, syncengine.WatchOptions) error {
				started <- struct{}{}
				<-release
				return nil
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	w.cancel = cancel
	go func() {
		w.errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()
	select {
	case <-started:
	case <-time.After(testWait):
		require.FailNow(t, "the drive did not start")
	}

	return w
}

func (w *stuckWatch) result(t *testing.T) error {
	t.Helper()

	select {
	case err := <-w.errCh:
		return err
	case <-time.After(testWait):
		require.FailNow(t, "the watch kept waiting for a drive that does not stop")
		return nil
	}
}

// A reload that has to stop a drive whose sync ignores the stop ends the
// watch with ErrSyncStuck instead of waiting for it forever, tells the
// client the sync is stopping, and starts nothing in the drive's place.
//
// Validates: R-6.3.9
func TestRunWatch_StuckRunnerOnReloadEndsTheWatchWithAnError(t *testing.T) {
	w := startStuckWatch(t, "personal:stuck-reload@example.com", "StuckReload")

	// A filter change is not restart-equivalent: the reload stops the runner
	// before it would start a new one.
	insertDriveConfigLine(t, w.cfgPath, w.cfg.StandaloneMounts[0].CanonicalID, `ignored_dirs = ["hidden"]`)
	status, answer := postControlJSONAnyStatus(t, w.cfg.ControlSocketPath, synccontrol.PathReload, nil)

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, synccontrol.ErrorInternal, answer.Code)
	require.ErrorIs(t, w.result(t), ErrSyncStuck)
	assert.Equal(t, int32(1), w.starts.Load(), "nothing started in place of the stuck drive")
	assert.Len(t, w.capture.AtLeast(slog.LevelError), 1, "the stuck drive is logged once, with the stacks")
	assert.NoFileExists(t, w.cfg.ControlSocketPath)
}

// A shutdown whose drive ignores the stop ends inside the bound with
// ErrSyncStuck, which is not a cancellation, so the command exits 1
// rather than 130 and a service manager restarts it.
//
// Validates: R-6.3.9, R-6.6.27
func TestRunWatch_StuckRunnerOnShutdownEndsTheWatchWithAnError(t *testing.T) {
	w := startStuckWatch(t, "personal:stuck-shutdown@example.com", "StuckShutdown")

	w.cancel()
	err := w.result(t)

	require.ErrorIs(t, err, ErrSyncStuck)
	require.NotErrorIs(t, err, context.Canceled)
	assert.Len(t, w.capture.AtLeast(slog.LevelError), 1)
	assert.NoFileExists(t, w.cfg.ControlSocketPath)
}

// A canceled runner whose final drain still finished cleanly must not wait
// on an event channel nobody reads: the loop that canceled it may be waiting
// for its done channel.
//
// Validates: R-6.3.9
func TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:full-channel@example.com", "Parent")
	drainMount := finalDrainMount(t, &parent, "binding-full-channel")

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	draining := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				close(draining)
				<-ctx.Done()
				return &syncengine.Report{}, nil
			},
		}, nil
	}

	unread := make(chan watchRunnerEvent)
	wr, err := orch.startWatchRunner(t.Context(), drainMount, 1, syncengine.SyncBidirectional, syncengine.WatchOptions{}, unread)
	require.NoError(t, err)
	<-draining
	wr.cancel()

	select {
	case <-wr.done:
	case <-time.After(testWait):
		require.FailNow(t, "a canceled runner blocked on the event channel")
	}
}

// The control socket's owner lock is the proof that this process owns its
// drives. It used to be released the moment the context was canceled, while
// the runners were still draining, so `drive reset-sync-state` and a second
// sync saw no owner during a slow drain.
//
// Validates: R-6.3.9, R-2.9.4
func TestRunWatch_OwnerLockHeldUntilRunnersDrain(t *testing.T) {
	rd := testStandaloneMount(t, "personal:lock-drain@example.com", "LockDrain")
	setupXDGIsolation(t, rd.CanonicalID)
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
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
	select {
	case <-started:
	case <-time.After(testWait):
		require.FailNow(t, "the drive did not start")
	}

	cancel()
	<-canceled
	lockPath := cfg.ControlSocketPath + controlLockSuffix
	// A window, kept until the orchestrator gives a signal: this test needs
	// the orchestrator's report that it is waiting for the canceled runner
	// to drain (after that point a released lock is a bug), and the
	// orchestrator reports no such thing yet.
	assert.Never(t, func() bool {
		lock, err := acquireOwnerLock(lockPath, cfg.ControlSocketPath)
		if err != nil {
			return false
		}
		assert.NoError(t, lock.release())
		return true
	}, 200*time.Millisecond, 10*time.Millisecond, "the owner lock was released while a drive was still draining")
	assert.FileExists(t, cfg.ControlSocketPath)

	close(release)
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(testWait):
		require.FailNow(t, "RunWatch did not stop in time")
	}
	assert.NoFileExists(t, cfg.ControlSocketPath)
	assert.NoFileExists(t, lockPath)
}
