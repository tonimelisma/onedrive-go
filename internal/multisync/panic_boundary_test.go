package multisync

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// watchOutcome is how a RunWatch call ended: its error, or the value of a
// panic that escaped it.
type watchOutcome struct {
	err     error
	escaped any
}

// runWatchCatchingPanics runs RunWatch on a goroutine of the test's that
// catches whatever escapes it, so a missing boundary fails the test instead of
// ending the test binary.
func runWatchCatchingPanics(t *testing.T, orch *Orchestrator) watchOutcome {
	t.Helper()

	done := make(chan watchOutcome, 1)
	ctx := t.Context()
	go func() {
		var outcome watchOutcome
		defer func() {
			outcome.escaped = recover()
			done <- outcome
		}()
		outcome.err = orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case outcome := <-done:
		return outcome
	case <-time.After(testWait):
		require.FailNow(t, "RunWatch did not return")
		return watchOutcome{}
	}
}

// panicErrorLogs are the Error records a recovered panic left.
func panicErrorLogs(capture *synctest.LogCapture) []synctest.CapturedRecord {
	var records []synctest.CapturedRecord
	for _, record := range capture.AtLeast(slog.LevelError) {
		if strings.HasPrefix(record.Message, "Internal error ") {
			records = append(records, record)
		}
	}

	return records
}

// RunWatch recovers a panic on the watch loop itself after its drain, logs it
// once with its stack, and returns it, so the CLI logs the stop and exits 1.
// Unrecovered, it would unwind the process past main with Go's exit status 2,
// no Error in the log file and no failure status for systemd.
//
// Validates: R-6.3.10
func TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError(t *testing.T) {
	rd := testStandaloneMount(t, "personal:loop-panic@example.com", "LoopPanic")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	cfg.WatchLifecycle = func(event WatchLifecycleEvent) {
		if event.Phase == WatchReady {
			panic("lifecycle hook exploded")
		}
	}

	orch := NewOrchestrator(cfg)
	engineStopped := make(chan struct{})
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				close(engineStopped)
				return ctx.Err()
			},
		}, nil
	}

	outcome := runWatchCatchingPanics(t, orch)

	require.Nil(t, outcome.escaped, "the panic escaped RunWatch")
	require.ErrorIs(t, outcome.err, panicguard.ErrPanic)
	require.ErrorContains(t, outcome.err, "internal error in the watch loop: lifecycle hook exploded")
	logged := panicErrorLogs(capture)
	require.Len(t, logged, 1, "the panic is logged once")
	assert.Equal(t, "lifecycle hook exploded", logged[0].Attrs["panic"])
	assert.Contains(t, logged[0].Attrs["stack"], "TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError",
		"the stack is the loop's, taken where it panicked")
	select {
	case <-engineStopped:
	default:
		require.Fail(t, "the drive was not stopped before RunWatch returned")
	}
	assert.NoFileExists(t, cfg.ControlSocketPath, "the control socket is closed")
}

// The drain and the socket close are registered before anything of the
// watch starts, so a panic at any point of its startup (here, in the start
// warning, after two drives started) stops what started and closes the
// socket, in that order, before RunWatch returns the panic.
//
// Validates: R-6.3.10, R-6.3.9
func TestRunWatch_APanicDuringStartupStopsTheDrivesThatStarted(t *testing.T) {
	first := testStandaloneMount(t, "personal:startup-first@example.com", "StartupFirst")
	second := testStandaloneMount(t, "personal:startup-second@example.com", "StartupSecond")
	cfgPath := writeTestConfig(t, first.CanonicalID, second.CanonicalID)
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, first, second)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	// A drive section the CLI could not read is announced as a start warning.
	cfg.InitialStartupResults = []MountStartupResult{{
		SelectionIndex: 2,
		Status:         MountStartupFatal,
		Err:            errors.New("unreadable drive section"),
	}}
	cfg.StartWarning = func(StartupWarning) {
		panic("start warning exploded")
	}

	orch := NewOrchestrator(cfg)
	var stopped atomic.Int32
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	outcome := runWatchCatchingPanics(t, orch)

	require.Nil(t, outcome.escaped, "the panic escaped RunWatch")
	require.ErrorIs(t, outcome.err, panicguard.ErrPanic)
	require.ErrorContains(t, outcome.err, "start warning exploded")
	assert.Equal(t, int32(2), stopped.Load(), "both drives stopped before RunWatch returned")
	assert.NoFileExists(t, cfg.ControlSocketPath, "the socket is closed before RunWatch returns")
	assert.Len(t, panicErrorLogs(capture), 1)
}

// panickingListener is a control socket listener whose Accept panics, as a
// bug in the server's accept path would. accepted, when set, is closed just
// before.
type panickingListener struct {
	net.Listener
	accepted chan struct{}
}

func (l panickingListener) Accept() (net.Conn, error) {
	if l.accepted != nil {
		close(l.accepted)
	}
	panic("accept exploded")
}

// A panic on the control server's goroutine travels the fatal channel a
// control server that stops on its own uses, so the watch ends with it after
// its drain.
//
// Validates: R-6.3.10, R-2.9.7
func TestRunWatch_ControlServerPanicEndsTheWatch(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	rd := testStandaloneMount(t, "personal:control-panic@example.com", "ControlPanic")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.controlListen = func(ctx context.Context, path string) (net.Listener, error) {
		listener, err := listenControlSocket(ctx, path)
		if err != nil {
			return nil, err
		}
		return panickingListener{Listener: listener}, nil
	}
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{}, nil
	}

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})

	require.ErrorIs(t, err, panicguard.ErrPanic)
	require.ErrorContains(t, err, "control channel stopped")
	require.ErrorContains(t, err, "accept exploded")
	logged := panicErrorLogs(capture)
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0].Attrs["stack"], "Accept")
	assert.NoFileExists(t, cfg.ControlSocketPath)
}

// A one-shot owner keeps doing what it does when its control server stops on
// its own: it finishes its pass. The panic is logged once with its stack.
//
// Validates: R-6.3.10, R-2.9.7
func TestRunOnce_ControlServerPanicIsLoggedAndThePassFinishes(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	rd := testStandaloneMount(t, "personal:oneshot-control-panic@example.com", "OneShotControlPanic")
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	accepted := make(chan struct{})
	orch.controlListen = func(ctx context.Context, path string) (net.Listener, error) {
		listener, err := listenControlSocket(ctx, path)
		if err != nil {
			return nil, err
		}
		return panickingListener{Listener: listener, accepted: accepted}, nil
	}
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				// The pass runs once the server has panicked, so its close
				// cannot come first.
				select {
				case <-accepted:
					return &syncengine.Report{}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Len(t, result.Reports, 1)
	require.NoError(t, result.Reports[0].Err, "the pass finished")
	require.NoError(t, result.Stuck)
	assert.Len(t, panicErrorLogs(capture), 1)
}

// A panic in the config file watcher is reported on the watch's fatal
// channel: a watcher that just stopped would leave a watch that never reloads
// its config again.
//
// Validates: R-6.3.10, R-4.4.3
func TestWatchConfigReloadTriggers_PanicIsReportedOnTheFatalChannel(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	capture, logger := synctest.NewLogCapture()
	fatal := make(chan error, 1)
	_, stop := watchConfigReloadTriggers(t.Context(), configReloadWatch{
		path:     filepath.Join(t.TempDir(), "config.toml"),
		interval: time.Hour,
		logger:   logger,
		fatal:    fatal,
		readStamp: func(string) configFileStamp {
			panic("stat exploded")
		},
	})
	defer stop()

	select {
	case err := <-fatal:
		require.ErrorIs(t, err, panicguard.ErrPanic)
		require.ErrorContains(t, err, "stat exploded")
	case <-time.After(testWait):
		require.Fail(t, "the watcher's panic was not reported")
	}
	assert.Len(t, panicErrorLogs(capture), 1)
}

// A panic while a drive's sync state closes leaves the store and its locks
// in an unknown state, exactly like a close that ran over, so it fails
// closed the same way: the runner stays, nothing starts under its ID, and the
// watch ends with the panic.
//
// Validates: R-6.3.10, R-6.3.9
func TestRetireWatchRunner_APanickingEngineCloseFailsClosed(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	h := newBackoffHarness(t)
	h.orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		h.starts.Add(1)
		return &mockEngine{
			closeFn: func(context.Context) error {
				panic("close exploded")
			},
		}, nil
	}
	never := make(chan time.Time)
	h.orch.retireTimer = func(time.Duration) (<-chan time.Time, func()) { return never, func() {} }

	h.reconcile()
	wr := h.runners[h.id]
	require.NotNil(t, wr)

	err := h.orch.retireWatchRunner(t.Context(), h.runners, h.id)

	require.ErrorIs(t, err, panicguard.ErrPanic)
	require.ErrorContains(t, err, "close exploded")
	assert.Same(t, wr, h.runners[h.id], "the runner stays, so nothing starts under its ID")
	require.ErrorIs(t, h.orch.retireWatchRunner(t.Context(), h.runners, h.id), panicguard.ErrPanic,
		"a later stop returns the same error without closing again")
	require.Len(t, h.capture.AtLeast(slog.LevelError), 1, "logged once, with its stack")
	assert.Contains(t, h.capture.AtLeast(slog.LevelError)[0].Attrs["stack"], "(*mockEngine).Close")
	delete(h.runners, h.id) // the harness cleanup must not close it again
}

// A one-shot pass whose engine close panics keeps its report, and the run
// fails with the panic, as with a close that runs over.
//
// Validates: R-6.3.10, R-6.3.9
func TestRunOnce_APanickingEngineCloseFailsTheRun(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	rd := testStandaloneMount(t, "personal:close-panic@example.com", "ClosePanic")
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			report: &syncengine.Report{},
			closeFn: func(context.Context) error {
				panic("close exploded")
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.ErrorIs(t, result.Stuck, panicguard.ErrPanic)
	require.ErrorContains(t, result.Stuck, "ClosePanic")
	require.Len(t, result.Reports, 1)
	require.NoError(t, result.Reports[0].Err, "the pass itself succeeded")
	assert.Len(t, panicErrorLogs(capture), 1)
}

// A shortcut's run is a goroutine of the one-shot pass. Its panic is that
// shortcut's error in the report, the pass still waits for every shortcut
// and returns, and the drive that owns it is unaffected.
//
// Validates: R-6.3.8, R-2.4.8
func TestRunOnce_AShortcutPanicIsThatShortcutsErrorAndThePassFinishes(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	parent := testStandaloneMount(t, "personal:child-panic@example.com", "Parent")
	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-panic", "Shortcuts/Panicky")
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.projectionKind() == MountProjectionChild {
			return &mockEngine{shouldPanic: true}, nil
		}
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				if err := req.Mount.shortcutChildWorkSink()(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
					return nil, err
				}
				return &syncengine.Report{}, nil
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.NoError(t, result.Stuck)
	var parentReport, childReport *MountReport
	for _, report := range result.Reports {
		switch report.Identity.ProjectionKind {
		case MountProjectionStandalone:
			parentReport = report
		case MountProjectionChild:
			childReport = report
		}
	}
	require.NotNil(t, parentReport)
	require.NoError(t, parentReport.Err, "the drive that owns the shortcut is unaffected")
	require.NotNil(t, childReport, "the shortcut has a report")
	require.ErrorIs(t, childReport.Err, panicguard.ErrPanic)
	require.ErrorContains(t, childReport.Err, "mock engine panic")
	assert.Len(t, panicErrorLogs(capture), 1)
}

// net/http recovers a control handler's panic per connection and logs it
// through Server.ErrorLog, which is the owner's logger: with the standard log
// package's default the record would bypass slog and the log file.
//
// Validates: R-6.3.10
func TestControlServer_HandlerPanicIsLoggedThroughTheOwnersLogger(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})
	serverCfg := testControlServerConfig(t, shortControlSocketPath(t), mux)
	serverCfg.logger = logger
	control, err := startControlSocketServer(t.Context(), serverCfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, control.Close(context.Background())) })

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/v1/status", http.NoBody)
	require.NoError(t, err)
	response, err := controlTestClient(serverCfg.path).Do(request)
	if err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.Error(t, err, "net/http drops the connection of a handler that panicked")

	require.Eventually(t, func() bool {
		for _, record := range capture.AtLeast(slog.LevelError) {
			if strings.Contains(record.Message, "handler exploded") {
				return true
			}
		}
		return false
	}, testWait, time.Millisecond, "the handler's panic reaches the owner's logger at Error")
}

// oneDriveWork is the one-shot work of the first drive of cfg, a report it
// has run and nothing else.
func oneDriveWork(t *testing.T, cfg *OrchestratorConfig, logger *slog.Logger) ([]*mountSpec, []indexedMountWork) {
	t.Helper()

	mounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	work := []indexedMountWork{{index: 0, work: mountWork{
		runner: &MountRunner{identity: mounts[0].identity(), displayName: mounts[0].displayName(), logger: logger},
		mount:  mounts[0],
		fn: func(context.Context) (*syncengine.Report, error) {
			return &syncengine.Report{}, nil
		},
	}}}

	return mounts, work
}

// The work after a drive's run starts its shortcuts under the coordinator's
// lock. A panic there, with the lock held, would leave every later report of
// the pass waiting on that lock. It is that drive's failure, joined into the
// report the drive's run already made, and the pass goes on.
//
// Validates: R-6.3.10, R-2.4.8
func TestRunIndexedMountWork_APanicWhileStartingShortcutsIsThatDrivesReport(t *testing.T) {
	t.Parallel()

	parent := testStandaloneMount(t, "personal:start-panic@example.com", "StartPanic")
	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-start-panic", "Shortcuts/StartPanic")
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	mounts, work := oneDriveWork(t, cfg, logger)
	coordinator := newOneShotChildRuns(orch, syncengine.SyncBidirectional, syncengine.RunOptions{}, mounts)
	coordinator.snapshots[mounts[0].id()] = processSnapshot(parent.CanonicalID.String(), child)
	// Marking the shortcuts started is the locked step; with no map to mark
	// them in it panics while it holds the lock.
	coordinator.started = nil

	reports := make([]*MountReport, 1)
	done := make(chan []*MountReport, 1)
	go func() {
		runIndexedMountWork(t.Context(), work, reports, coordinator)
		coordinator.wait()
		done <- coordinator.reports()
	}()

	var shortcutReports []*MountReport
	select {
	case shortcutReports = <-done:
	case <-time.After(testWait):
		require.FailNow(t, "the pass hung on the lock the panic left held")
	}
	require.NotNil(t, reports[0])
	require.ErrorIs(t, reports[0].Err, panicguard.ErrPanic)
	assert.NotNil(t, reports[0].Report, "the drive's own report stands beside the panic")
	assert.Empty(t, shortcutReports, "the drive has one report, not a second one for the panic")
	assert.Len(t, panicErrorLogs(capture), 1)
}

// A drive's run is recovered inside MountRunner.run; a panic outside it, with
// the drive's report slot still empty, fills the slot with the panic, so no
// drive of the pass goes without a report.
//
// Validates: R-6.3.10
func TestRunIndexedMountWork_APanicBeforeTheRunFillsTheDrivesReport(t *testing.T) {
	t.Parallel()

	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	parent := testStandaloneMount(t, "personal:runner-panic@example.com", "RunnerPanic")
	cfg := testOrchestratorConfig(t, parent)
	_, work := oneDriveWork(t, cfg, slog.New(slog.DiscardHandler))
	// Without a runner the run panics before its own recover is registered,
	// and the record goes to the default logger.
	work[0].work.runner = nil

	reports := make([]*MountReport, 1)
	runIndexedMountWork(t.Context(), work, reports, nil)

	require.NotNil(t, reports[0], "the drive still has a report")
	require.ErrorIs(t, reports[0].Err, panicguard.ErrPanic)
}

// A panic in the work around a drive's shortcuts (here, building a
// shortcut's engine, which no shortcut's own run covers) is a report against
// that drive. The other drive of the pass finishes, and the pass returns.
//
// Validates: R-6.3.10, R-2.4.8
func TestRunOnce_APanicAroundADrivesShortcutsIsThatDrivesReport(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	withShortcuts := testStandaloneMount(t, "personal:shortcut-setup-panic@example.com", "WithShortcuts")
	plain := testStandaloneMount(t, "personal:plain-drive@example.com", "Plain")
	child := testChildRecord(t, mountID(withShortcuts.CanonicalID.String()), "binding-setup-panic", "Shortcuts/Setup")
	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, withShortcuts, plain)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)
	plainSynced := make(chan struct{})
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch {
		case req.Mount.projectionKind() == MountProjectionChild:
			panic("shortcut engine exploded")
		case req.Mount.id() == mountID(plain.CanonicalID.String()):
			return &mockEngine{runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
				close(plainSynced)
				return &syncengine.Report{}, nil
			}}, nil
		default:
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if err := req.Mount.shortcutChildWorkSink()(ctx, processSnapshot(withShortcuts.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
					return &syncengine.Report{}, nil
				},
			}, nil
		}
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	select {
	case <-plainSynced:
	default:
		require.Fail(t, "the other drive did not sync")
	}
	var panicked []*MountReport
	finished := 0
	for _, report := range result.Reports {
		if report.Err == nil {
			finished++
			continue
		}
		panicked = append(panicked, report)
	}
	assert.Equal(t, 2, finished, "both drives' own runs finished")
	require.Len(t, panicked, 1)
	require.ErrorIs(t, panicked[0].Err, panicguard.ErrPanic)
	assert.Equal(t, "WithShortcuts", panicked[0].DisplayName, "the panic is reported against the drive that owns the shortcuts")
	assert.Len(t, panicErrorLogs(capture), 1)
}
