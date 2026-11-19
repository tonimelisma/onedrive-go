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
)

const watchGoroutinesBootstrapToken = "bootstrap-token"

// bootstrapThenHoldDeltaMock answers the bootstrap's full enumeration, then
// hands every later delta call, the remote observer's, to observe.
func bootstrapThenHoldDeltaMock(observe func(ctx context.Context) error) *engineMockClient {
	return &engineMockClient{
		deltaFn: func(ctx context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			if token == "" {
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
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
	recorder := attachDebugEventRecorderWithHook(eng, func(event engineDebugEvent) {
		if event.Type == engineDebugEventBootstrapQuiesced {
			fault.arm("FROM mount_boundaries ORDER BY path", errInjectedFault)
		}
	})

	err := runWithin(t, t.Context(), "the watch to fail on the local set-up", func(ctx context.Context) error {
		return eng.RunWatch(ctx, SyncBidirectional, quietWatchOptions())
	})
	require.ErrorIs(t, err, errInjectedFault)
	assert.Equal(t, 1, fault.fired(), "the mount boundaries read failed")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted
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
	eng.localWatcherFactory = func() (fsWatcher, error) {
		<-polling
		panic("local watcher exploded")
	}
	stopWaiting := installAfterFuncCreatedSignal(eng, watchGoroutineStopTimeout)

	_, done := runWatchInBackground(t, eng, SyncBidirectional, quietWatchOptions())
	receiveWithin(t, stopWaiting, "the watch's stop to wait for its goroutines")

	select {
	case <-done:
		assert.Fail(t, "the watch returned while its remote observer was still in its delta call")
	default:
	}
	close(release)
	err := receiveWithin(t, done, "the watch to return after its remote observer")
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
	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, mock, logger)
	clock := newManualClock(time.Now())
	installManualClockForTestEngine(eng, clock)
	stopWaiting := installAfterFuncCreatedSignal(eng, watchGoroutineStopTimeout)
	recorder := attachDebugEventRecorder(eng)
	t.Cleanup(func() { close(release) })

	cancel, done := runWatchInBackground(t, eng, SyncBidirectional, quietWatchOptions())
	receiveWithin(t, polling, "the remote observer's first poll")
	cancel()
	receiveWithin(t, stopWaiting, "the watch's stop to wait for its goroutines")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverExited && event.Observer == engineDebugObserverLocal
	}, "the local observer, which honors its context, to exit")
	clock.Advance(watchGoroutineStopTimeout)

	require.NoError(t, receiveWithin(t, done, "the watch to return once the bound has passed"))
	output := logs.String()
	assert.Contains(t, output, "Watch goroutines did not stop in time")
	assert.Contains(t, output, `still_running="[`+string(watchGoroutineRemoteObserver)+`]"`,
		"the Warn names the remote observer alone: the local one stopped")
	assert.Equal(t, 1, countLevel(output, "WARN"), "one Warn")
}

// The orchestrator's final-drain acknowledgement, through the ack handle, and
// a full remote refresh both rewrite the protected roots, from two goroutines
// beside the watch loop. Run under -race: both write through the lock.
//
// Validates: R-6.3.4
func TestAckHandle_ConcurrentWithFullRefresh_IsRaceFree(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.shortcutNamespaceID = shortcutNamespaceTestID
	seedRemovedSharedFolders(t, eng.Engine, syncRoot)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	t.Cleanup(func() { rt.resources.goroutines.stop(rt.deps.clock, rt.deps.logger) })
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	runFullRemoteRefreshAsyncForTest(t, eng, t.Context(), bl)
	_, ackErr := eng.ShortcutChildAckHandle().AcknowledgeChildFinalDrain(t.Context(), ShortcutChildDrainAck{
		Ref: newShortcutChildAckRef("binding-1"),
	})
	require.NoError(t, ackErr)

	result := receiveWithin(t, rt.resources.refreshResults, "the full remote refresh's result")
	require.NoError(t, result.fatalErr)
}
