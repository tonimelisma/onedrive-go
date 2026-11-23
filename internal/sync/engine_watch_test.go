package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ---------------------------------------------------------------------------
// RunWatch tests
// ---------------------------------------------------------------------------

type stubSocketIOWakeSource struct {
	started chan struct{}
	runFn   func(context.Context, chan<- struct{}) error
}

func (s *stubSocketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
	if s.runFn != nil {
		return s.runFn(ctx, wakes)
	}

	select {
	case s.started <- struct{}{}:
	default:
	}

	<-ctx.Done()
	return nil
}

// Validates: R-2.8.3
func TestResolveDebounce_DefaultIsFiveSeconds(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5*time.Second, resolveDebounce(WatchOptions{}))
	assert.Equal(t, 1500*time.Millisecond, resolveDebounce(WatchOptions{Debounce: 1500 * time.Millisecond}))
}

// Validates: R-2.8.3, R-6.10.10
// TestRunWatch_ContextCancel verifies that canceling the context causes
// RunWatch to return nil (clean shutdown), including during bootstrap.
func TestRunWatch_ContextCancel(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(ctx context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncBidirectional, WatchOptions{
		// Use very long intervals so observers don't fire during test.
		PollInterval: 1 * time.Hour,
		Debounce:     1 * time.Hour,
	})

	syncfixture.ReceiveWithin(t, started, "RunWatch to enter bootstrap observation")
	syncfixture.StopWatch(t, cancel, done)

	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted
	}), "observers must not start after bootstrap cancellation")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketEnabledStartsWakeSource(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.enableWebsocket = true
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.hooks.socketIOWakeSourceFactory = func(_ syncdomain.SocketIOEndpointFetcher, _ driveid.ID, opts syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{
			started: started,
			runFn: func(ctx context.Context, _ chan<- struct{}) error {
				require.NotNil(t, opts.LifecycleHook)
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventStarted,
					DriveID: driveID.String(),
				})
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventConnected,
					DriveID: driveID.String(),
					SID:     "sid-1",
				})
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventStopped,
					DriveID: driveID.String(),
				})
				return nil
			},
		}
	}

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncDownloadOnly, quietWatchOptions())

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWebsocketWakeSourceStarted && event.DriveID == driveID.String()
	}, "websocket wake source started")
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWebsocketConnected &&
			event.DriveID == driveID.String() &&
			event.Note == "sid=sid-1"
	}, "websocket connected")

	syncfixture.StopWatch(t, cancel, done)
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWebsocketWakeSourceStopped && event.DriveID == driveID.String()
	}, "websocket wake source stopped")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketDisabledKeepsPollingOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)
	wakeSourcesMade := countWakeSourcesMade(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncDownloadOnly, quietWatchOptions())

	waitForWakeSourceDecision(t, recorder)
	assert.Zero(t, wakeSourcesMade.Load(), "wake source should not start when websocket is disabled")

	syncfixture.StopWatch(t, cancel, done)
	assert.Zero(t, wakeSourcesMade.Load(), noWakeSourceAfterTheDecision)
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWebsocketWakeSourceStarted ||
			event.Type == syncdomain.EngineDebugEventWebsocketConnected
	}), "disabled websocket mode must not emit websocket lifecycle events")
}

// noWakeSourceAfterTheDecision is the failure of a wake source made after the
// watch decided against one, read once the watch has returned.
const noWakeSourceAfterTheDecision = "no wake source was made for the rest of the watch either"

// countWakeSourcesMade replaces eng's wake-source factory with one that
// counts its calls: the call is the engine's decision to start a wake source,
// made before the source's goroutine runs.
func countWakeSourcesMade(eng *testEngine) *atomic.Int32 {
	made := &atomic.Int32{}
	eng.hooks.socketIOWakeSourceFactory = func(_ syncdomain.SocketIOEndpointFetcher, _ driveid.ID, _ syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		made.Add(1)
		return &stubSocketIOWakeSource{started: make(chan struct{}, 1)}
	}

	return made
}

// waitForWakeSourceDecision waits until the watch has decided whether to
// start a wake source. The remote observer is started with its wake source,
// if any, before the local observer, so the local observer's start is that
// signal: a wake source the watch decided on has been made by then.
func waitForWakeSourceDecision(t *testing.T, recorder *syncfixture.DebugEventRecorder) {
	t.Helper()

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started, after the remote observer and its wake source")
}

// Validates: R-2.8.5
func TestRunWatch_MountRootKeepsPollingOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.enableWebsocket = true
	eng.remoteRootItemID = "mount-root-root"
	recorder := attachDebugEventRecorder(eng)
	wakeSourcesMade := countWakeSourcesMade(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncDownloadOnly, quietWatchOptions())

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWebsocketFallback && event.Note == "mount_root"
	}, "websocket mount-root fallback")
	waitForWakeSourceDecision(t, recorder)
	assert.Zero(t, wakeSourcesMade.Load(), "wake source should not start for mount-root watch")

	syncfixture.StopWatch(t, cancel, done)
	assert.Zero(t, wakeSourcesMade.Load(), noWakeSourceAfterTheDecision)
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_CancellationWinsOverFinalObserverExit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	cancel()
	require.NoError(t, syncfixture.ReceiveWithin(t, done, "RunWatch to return after cancellation"),
		"RunWatch should prefer graceful shutdown over all-observers-exited")

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverExited && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer exited")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventWatchStopped
	}, "watch stopped")
}

// Validates: R-2.1.4
// TestRunWatch_UploadOnly_StartsRemoteObserver verifies that upload-only mode
// still observes remote truth via delta polling.
func TestRunWatch_UploadOnly_StartsRemoteObserver(t *testing.T) {
	t.Parallel()

	var deltaCalls atomic.Int32

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalls.Add(1)
			return syncfixture.DeltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, WatchOptions{
		PollInterval: 50 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
	})

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	syncfixture.StopWatch(t, cancel, done)

	assert.Positive(t, deltaCalls.Load(), "upload-only watch should still issue delta calls")
	assert.True(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverRemote
	}), "upload-only watch must start a remote observer")
}

// Validates: R-2.14.3
func TestWatchRuntime_HandleMaintenanceTick_EmitsDebugEventWithoutSQLiteProbe(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	recorder := attachDebugEventRecorder(eng)
	ctx := t.Context()
	newTestWatchState(t, eng)

	testWatchRuntime(t, eng).handleMaintenanceTick(ctx)

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventMaintenanceTickHandled
	}, "maintenance tick handled")
}

// TestRunWatch_ProcessBatch_EmptyPlan verifies that an empty plan (all
// changes classify to no-op) is handled gracefully.
func TestRunWatch_ProcessBatch_EmptyPlan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	content := "hello"
	contentHash := syncfixture.HashContentQuickXor(t, content)

	// Seed baseline with a synced file.
	seedOutcomes := []syncdomain.ActionOutcome{{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-as",
		ItemType:        syncdomain.ItemTypeFile,
		RemoteHash:      contentHash,
		LocalHash:       contentHash,
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")
	syncfixture.WriteLocalFile(t, syncRoot, "already-synced.txt", content)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-as",
		Path:     "already-synced.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     contentHash,
		Size:     5,
	}}, ""))

	rt := testWatchRuntime(t, eng)
	applied, err := rt.runSteadyStateReplan(ctx, &watchPipeline{
		bl:   bl,
		mode: syncdomain.SyncBidirectional,
	}, syncobserve.DirtyBatch{})
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Empty(t, rt.currentOutbox())
}

// Validates: R-2.8.6
// TestRunWatch_BusyRuntimeQueuesPendingReplanInsteadOfOverlappingPrepare
// verifies that a replan trigger observed while work is still running is
// queued for the next linear replan cycle instead of mutating the current
// runtime in place.
func TestRunWatch_BusyRuntimeQueuesPendingReplanInsteadOfOverlappingPrepare(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.sched.Running(&syncdomain.TrackedAction{ID: 1 << 40, Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "running.txt"}})

	err = rt.handleWatchReplanReady(ctx, &watchPipeline{
		bl:   bl,
		mode: syncdomain.SyncBidirectional,
	}, syncobserve.DirtyBatch{})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox())
	assert.True(t, rt.hasPendingReplan())

	queued, ok := rt.takePendingReplan()
	require.True(t, ok)
	assert.False(t, queued.FullRefresh)
}

// Validates: R-2.1.3
// TestRunWatch_DownloadOnly_StartsLocalObserver verifies that download-only
// mode still observes local truth even though uploads stay suppressed.
func TestRunWatch_DownloadOnly_StartsLocalObserver(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncDownloadOnly, WatchOptions{
		PollInterval: 1 * time.Hour,
		Debounce:     10 * time.Millisecond,
	})

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	// Wait for the local observer itself: canceling as soon as the remote one
	// starts raced its startup (the remote start's state read saw the
	// cancellation and the runtime stopped before the local observer began).
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	// Create a local file. Download-only still observes it, but upload actions
	// remain suppressed by mode admission.
	syncfixture.WriteLocalFile(t, syncRoot, "local-only.txt", "should-be-ignored")

	syncfixture.StopWatch(t, cancel, done)

	bl, err := eng.baseline.Load(context.Background())
	require.NoError(t, err)
	_, found := bl.GetByPath("local-only.txt")
	assert.False(t, found, "download-only watch mode must ignore local-only files created after bootstrap")
	assert.True(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}), "download-only watch must start a local observer")
}

// Validates: R-2.8.22
// TestRunWatch_WatchLimitExhausted_FallsBackToPolling verifies that when the
// local observer returns ErrLocalWatchUnavailable, the engine does NOT consider
// the observer dead. Instead it falls back to periodic full scanning and
// RunWatch continues until the context is canceled.
func TestRunWatch_WatchLimitExhausted_FallsBackToPolling(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create a subdirectory so the ENOSPC watcher has something to fail on.
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))

	// Inject a watcher factory that returns ENOSPC after the first Add (root).
	watcher := syncfixture.NewEnospcWatcher(1)
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, WatchOptions{
		PollInterval: 100 * time.Millisecond, // short for fast test
		Debounce:     10 * time.Millisecond,
	})

	syncfixture.ReceiveWithin(t, watcher.Failures(), "the local watcher to hit ENOSPC")

	// RunWatch returns nil (clean shutdown), not an "all observers exited" error.
	cancel()
	require.NoError(t, syncfixture.ReceiveWithin(t, done, "RunWatch to return after cancellation"),
		"RunWatch should return nil on clean shutdown with fallback polling")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_ShutdownStopsRetryAndTrialTimers(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	clock := syncfixture.NewManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	ctx := t.Context()
	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           syncdomain.SKService(),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   eng.nowFunc().Add(5 * time.Second),
	})
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("held.txt", "", syncdomain.ActionUpload), syncdomain.SKService(), nil)
	require.NoError(t, err)

	_, err = eng.baseline.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("retry.txt", "", syncdomain.ActionDownload), func(_ int) time.Duration {
		return 5 * time.Second
	})
	require.NoError(t, err)
	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	cancel()
	done = recorder.WaitUntilSeenOrExit(t, done, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventShutdownStarted
	}, "shutdown started")

	clock.Advance(10 * time.Second)
	require.NoError(t, syncfixture.ReceiveWithin(t, done, "the watch loop to exit once its timers were advanced"))

	recorder.RequireOrderedSubsequence(t, []func(syncdomain.EngineDebugEvent) bool{
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventShutdownStarted
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventWatchStopped
		},
	}, "local observer start, shutdown, and watch stop should occur in order")
	for _, forbidden := range []syncdomain.EngineDebugEventType{
		syncdomain.EngineDebugEventRetryTimerFired,
		syncdomain.EngineDebugEventRetryHeldReleaseStarted,
		syncdomain.EngineDebugEventTrialTimerFired,
		syncdomain.EngineDebugEventTrialHeldReleaseStarted,
	} {
		recorder.RequireNoEventAfter(t, func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventShutdownStarted
		}, func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == forbidden
		}, fmt.Sprintf("%s must not occur after shutdown starts", forbidden))
	}
}

func installTickerCreatedSignal(eng *testEngine, interval time.Duration) <-chan struct{} {
	tickerCreated := make(chan struct{})
	var tickerCreatedOnce atomic.Bool
	observeEngineClock(eng.Engine).OnNewTicker = func(nextInterval time.Duration) {
		if nextInterval == interval && tickerCreatedOnce.CompareAndSwap(false, true) {
			close(tickerCreated)
		}
	}

	return tickerCreated
}

func installAfterFuncCreatedSignal(eng *testEngine, delay time.Duration) <-chan struct{} {
	timerCreated := make(chan struct{})
	var timerCreatedOnce atomic.Bool
	observeEngineClock(eng.Engine).OnAfterFunc = func(nextDelay time.Duration) {
		if nextDelay == delay && timerCreatedOnce.CompareAndSwap(false, true) {
			close(timerCreated)
		}
	}

	return timerCreated
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_ShutdownAfterCommittedRefreshDoesNotDropAppliedBatch(t *testing.T) {
	t.Parallel()

	refreshStarted := make(chan struct{})
	refreshReleased := make(chan struct{})
	var refreshStartedOnce atomic.Bool
	var deltaCalls atomic.Int32
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return syncfixture.DeltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(syncfixture.EngineTestDriveID)},
				}, "bootstrap-token"), nil
			}
			if refreshStartedOnce.CompareAndSwap(false, true) {
				close(refreshStarted)
			}
			<-refreshReleased
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(syncfixture.EngineTestDriveID)},
				{ID: "late-item", Name: "late.txt", ParentID: "root", DriveID: driveid.New(syncfixture.EngineTestDriveID), Size: 10, QuickXorHash: "late-hash"},
			}, "refresh-token"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	clock := syncfixture.NewManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	watcher := newSignalingWatcher()
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}
	refreshTimerCreated := installAfterFuncCreatedSignal(eng, 15*time.Minute)
	syncstoretest.SaveObservationCursorForTest(t, eng.baseline, t.Context(), syncfixture.EngineTestDriveID, "seed-token")
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(
		t.Context(),
		driveid.New(syncfixture.EngineTestDriveID),
		clock.Now().Add(-syncdomain.FullRemoteRefreshInterval+15*time.Minute),
		syncdomain.RemoteObservationModeDelta,
	))

	watchCtx, cancel := context.WithCancel(t.Context())
	recorder := attachDebugEventRecorderWithHook(eng, func(event syncdomain.EngineDebugEvent) {
		if event.Type == syncdomain.EngineDebugEventRemoteRefreshCommitted {
			cancel()
		}
	})

	// The watch runs on watchCtx, which the recorder's hook cancels.
	_, done := syncfixture.RunInBackground(t, watchCtx, func(ctx context.Context) error {
		return eng.RunWatch(ctx, syncdomain.SyncUploadOnly, quietWatchOptions())
	})

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")
	syncfixture.ReceiveWithin(t, watcher.Added(), "local watch setup to add a watcher")
	syncfixture.ReceiveWithin(t, refreshTimerCreated, "the refresh timer to be created")

	clock.Advance(15 * time.Minute)
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventRemoteRefreshStarted
	}, "full remote refresh started")
	syncfixture.ReceiveWithin(t, refreshStarted, "the full remote refresh's delta fetch to start")

	close(refreshReleased)

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventShutdownStarted
	}, "shutdown started")

	require.NoError(t, syncfixture.ReceiveWithin(t, done, "the watch loop to exit after shutdown during committed refresh application"))

	recorder.RequireOrderedSubsequence(t, []func(syncdomain.EngineDebugEvent) bool{
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventRemoteRefreshStarted
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventRemoteRefreshApplied
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventShutdownStarted
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventWatchStopped
		},
	}, "loop-owned refresh application should complete before shutdown drains the watch loop")
	recorder.RequireEventCount(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventRemoteRefreshDroppedOnShutdown
	}, 0, "applied full remote refresh batches should not later be dropped as pending shutdown work")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_FallbackSleepHonorsCancellation(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))
	recorder := attachDebugEventRecorder(eng)

	clock := syncfixture.NewManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	clock.SetJitter(5 * time.Second)
	installManualClockForTestEngine(eng, clock)

	sleepStarted := make(chan struct{})
	var sleepStartedOnce atomic.Bool
	observeEngineClock(eng.Engine).OnSleep = func(time.Duration) {
		if sleepStartedOnce.CompareAndSwap(false, true) {
			close(sleepStarted)
		}
	}

	watcher := syncfixture.NewEnospcWatcher(1)
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}
	degradedInterval := syncobserve.LocalWatchHealthySafetyScanInterval
	tickerCreated := installTickerCreatedSignal(eng, degradedInterval)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, WatchOptions{
		PollInterval: 1 * time.Second,
		Debounce:     5 * time.Millisecond,
	})

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverFallbackStarted
	}, "fallback started")
	syncfixture.ReceiveWithin(t, tickerCreated, "the fallback ticker to be created")
	clock.Advance(degradedInterval)
	syncfixture.ReceiveWithin(t, sleepStarted, "the fallback jitter sleep to start")

	// The sleep is a manual clock's: only cancellation can end it.
	syncfixture.StopWatch(t, cancel, done)

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverFallbackStopped
	}, "fallback stopped")
}

// ---------------------------------------------------------------------------
// Plan invariant guard tests
// ---------------------------------------------------------------------------

// TestExecutePlan_ActionsDepsLengthMismatch verifies that executePreparedPlan
// returns cleanly (no panic) when plan.Actions and plan.Deps have mismatched
// lengths.
func TestExecutePlan_ActionsDepsLengthMismatch(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngine(t, mock)

	// Create a plan with mismatched Actions and Deps.
	plan := &syncdomain.ActionPlan{
		Actions: []syncdomain.Action{
			{Type: syncdomain.ActionDownload, Path: "file.txt"},
			{Type: syncdomain.ActionDownload, Path: "file2.txt"},
		},
		Deps: [][]int{{1}}, // only 1 dep entry for 2 actions
	}

	report := &Report{}

	// Should return cleanly without panic.
	require.NoError(t, newOneShotRunner(eng.Engine).executePreparedPlan(t.Context(), &runtimePlan{
		Plan:   plan,
		Report: report,
	}, nil))

	// Invariant violation should surface in the report.
	assert.Equal(t, len(plan.Actions), report.Failed)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Error(), "invariant violation")
}

// ---------------------------------------------------------------------------
// Close() cleanup and idempotency
// ---------------------------------------------------------------------------

// TestEngine_Close_CleansStaleAndIsIdempotent verifies that Close() cleans
// stale session files and remains idempotent even when a test runtime exists.
func TestEngine_Close_CleansStaleAndIsIdempotent(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	dataDir := filepath.Join(tmpDir, "data")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	require.NoError(t, os.MkdirAll(dataDir, 0o750))

	logger := syncfixture.TestLogger(t)
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DataDir:   dataDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	testEng := newFlowBackedTestEngine(eng)

	// Register a test runtime to prove Close operates on engine-owned resources
	// only; watch observers now belong to the runtime and are cleaned up by the
	// watch coordinator, not by Engine.Close.
	setupWatchEngine(t, testEng)
	testWatchRuntime(t, testEng).resources.remoteObs = &syncobserve.RemoteObserver{}
	testWatchRuntime(t, testEng).resources.localObs = &syncobserve.LocalObserver{}

	// First Close should succeed.
	require.NoError(t, eng.Close(t.Context()))

	// Second Close must not panic (idempotency). The baseline DB is already
	// closed. A second Close should still be a clean no-op.
	assert.NotPanics(t, func() {
		assert.NoError(t, eng.Close(t.Context()))
	}, "second Close must not panic")
}
