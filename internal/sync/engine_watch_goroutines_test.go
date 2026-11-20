package sync

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

const watchGoroutinesBootstrapToken = "bootstrap-token"

// bootstrapThenHoldDeltaMock answers the bootstrap's full enumeration, then
// hands every later delta call, the remote observer's, to observe.
func bootstrapThenHoldDeltaMock(observe func(ctx context.Context) error) *syncfixture.EngineMockClient {
	return &syncfixture.EngineMockClient{
		DeltaFn: func(ctx context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			if token == "" {
				return syncfixture.DeltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(syncfixture.EngineTestDriveID)},
				}, watchGoroutinesBootstrapToken), nil
			}

			return nil, observe(ctx)
		},
	}
}

// A store read the local observer needs fails after the bootstrap. The watch
// fails with it and starts no observer: the remote one used to start before
// that read and kept polling after the watch had returned.
//
// Validates: R-6.3.4
func TestStartObservers_LocalSetupFailureStartsNoObserver(t *testing.T) {
	t.Parallel()

	var observerCalls atomic.Int32
	mock := bootstrapThenHoldDeltaMock(func(ctx context.Context) error {
		observerCalls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})
	eng, fault := newTestEngineOnFaultStore(t, mock, nil)
	recorder := attachDebugEventRecorderWithHook(eng, func(event syncdomain.EngineDebugEvent) {
		if event.Type == syncdomain.EngineDebugEventBootstrapQuiesced {
			fault.Arm("FROM mount_boundaries ORDER BY path", syncfixture.ErrInjectedFault)
		}
	})

	err := syncfixture.RunWithin(t, t.Context(), "the watch to fail on the local set-up", func(ctx context.Context) error {
		return eng.RunWatch(ctx, syncdomain.SyncBidirectional, quietWatchOptions())
	})
	require.ErrorIs(t, err, syncfixture.ErrInjectedFault)
	assert.Equal(t, 1, fault.Fired(), "the mount boundaries read failed")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted
	}), "no observer started")
	assert.Zero(t, observerCalls.Load(), "the remote fetcher saw no observer poll")
}

// A watch that fails returns only after its remote observer has: the local
// observer panics, which fails the watch without its shutdown drain, while
// the remote observer is still inside a delta call, and the watch waits for
// that call to end. It used to return with the remote observer still running.
//
// Validates: R-6.3.4, R-6.3.8
func TestRunWatch_ReturnsOnlyAfterObserversHaveExited(t *testing.T) {
	t.Parallel()

	polling := make(chan struct{})
	release := make(chan struct{})
	var observerReturned atomic.Bool
	mock := bootstrapThenHoldDeltaMock(func(ctx context.Context) error {
		close(polling)
		<-ctx.Done()
		<-release
		observerReturned.Store(true)
		return ctx.Err()
	})
	eng, _ := newTestEngine(t, mock)
	eng.localWatcherFactory = func() (syncobserve.FsWatcher, error) {
		<-polling
		panic("local watcher exploded")
	}
	stopWaiting := installAfterFuncCreatedSignal(eng, watchGoroutineStopTimeout)

	_, done := runWatchInBackground(t, eng, syncdomain.SyncBidirectional, quietWatchOptions())
	syncfixture.ReceiveWithin(t, stopWaiting, "the watch's stop to wait for its goroutines")

	select {
	case <-done:
		assert.Fail(t, "the watch returned while its remote observer was still in its delta call")
	default:
	}
	close(release)
	err := syncfixture.ReceiveWithin(t, done, "the watch to return after its remote observer")
	require.ErrorContains(t, err, "local watcher exploded")
	assert.True(t, observerReturned.Load(), "the remote observer returned before the watch did")
}

// A remote observer that ignores its context does not hold a stopping
// watch's return for ever: the drain, then the stop, wait for it within one
// bound on the engine's clock; once it has passed, one Warn names the kind
// still running and the watch returns.
//
// Validates: R-6.3.9
func TestRunWatch_ObserverThatIgnoresItsContextIsNamedAfterTheBound(t *testing.T) {
	t.Parallel()

	polling := make(chan struct{})
	release := make(chan struct{})
	mock := bootstrapThenHoldDeltaMock(func(context.Context) error {
		close(polling)
		<-release
		return nil
	})
	logger, logs := syncfixture.NewCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, mock, logger)
	clock := syncfixture.NewManualClock(time.Now())
	installManualClockForTestEngine(eng, clock)
	stopWaiting := installAfterFuncCreatedSignal(eng, watchGoroutineStopTimeout)
	recorder := attachDebugEventRecorder(eng)
	t.Cleanup(func() { close(release) })

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncBidirectional, quietWatchOptions())
	syncfixture.ReceiveWithin(t, polling, "the remote observer's first poll")
	cancel()
	syncfixture.ReceiveWithin(t, stopWaiting, "the watch's stop to wait for its goroutines")
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverExited && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "the local observer, which honors its context, to exit")
	clock.Advance(watchGoroutineStopTimeout)

	require.NoError(t, syncfixture.ReceiveWithin(t, done, "the watch to return once the bound has passed"))
	output := logs.String()
	assert.Contains(t, output, "Watch goroutines did not stop in time")
	assert.Contains(t, output, `still_running="[`+string(watchGoroutineRemoteObserver)+`]"`,
		"the Warn names the remote observer alone: the local one stopped")
	assert.Equal(t, 1, syncfixture.CountLevel(output, "WARN"), "one Warn")
}

// The orchestrator's final-drain acknowledgement, through the ack handle, and
// a full remote refresh both rewrite the protected roots, from two goroutines
// beside the watch loop. Run under -race: both write through the lock.
//
// Validates: R-6.3.4
func TestAckHandle_ConcurrentWithFullRefresh_IsRaceFree(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	seedRemovedSharedFolders(t, eng.Engine, syncRoot)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	t.Cleanup(func() { rt.resources.goroutines.stop(rt.deps.clock, rt.deps.logger) })
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	runFullRemoteRefreshAsyncForTest(t, eng, t.Context(), bl)
	_, ackErr := eng.ShortcutChildAckHandle().AcknowledgeChildFinalDrain(t.Context(), syncshortcut.ShortcutChildDrainAck{
		Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
	})
	require.NoError(t, ackErr)

	result := syncfixture.ReceiveWithin(t, rt.resources.refreshResults, "the full remote refresh's result")
	require.NoError(t, result.FatalErr)
}
