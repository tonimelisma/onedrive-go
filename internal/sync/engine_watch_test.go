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

	eng := &Engine{}
	assert.Equal(t, 5*time.Second, eng.resolveDebounce(WatchOptions{}))
	assert.Equal(t, 1500*time.Millisecond, eng.resolveDebounce(WatchOptions{Debounce: 1500 * time.Millisecond}))
}

// Validates: R-2.8.3, R-6.10.10
// TestRunWatch_ContextCancel verifies that canceling the context causes
// RunWatch to return nil (clean shutdown), including during bootstrap.
func TestRunWatch_ContextCancel(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	mock := &engineMockClient{
		deltaFn: func(ctx context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, SyncBidirectional, WatchOptions{
		// Use very long intervals so observers don't fire during test.
		PollInterval: 1 * time.Hour,
		Debounce:     1 * time.Hour,
	})

	receiveWithin(t, started, "RunWatch to enter bootstrap observation")
	stopWatch(t, cancel, done)

	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted
	}), "observers must not start after bootstrap cancellation")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketEnabledStartsWakeSource(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.enableWebsocket = true
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ socketIOEndpointFetcher, _ driveid.ID, opts socketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{
			started: started,
			runFn: func(ctx context.Context, _ chan<- struct{}) error {
				require.NotNil(t, opts.LifecycleHook)
				opts.LifecycleHook(socketIOLifecycleEvent{
					Type:    socketIOLifecycleEventStarted,
					DriveID: driveID.String(),
				})
				opts.LifecycleHook(socketIOLifecycleEvent{
					Type:    socketIOLifecycleEventConnected,
					DriveID: driveID.String(),
					SID:     "sid-1",
				})
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				opts.LifecycleHook(socketIOLifecycleEvent{
					Type:    socketIOLifecycleEventStopped,
					DriveID: driveID.String(),
				})
				return nil
			},
		}
	}

	cancel, done := runWatchInBackground(t, eng, SyncDownloadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStarted && event.DriveID == driveID.String()
	}, "websocket wake source started")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketConnected &&
			event.DriveID == driveID.String() &&
			event.Note == "sid=sid-1"
	}, "websocket connected")

	stopWatch(t, cancel, done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStopped && event.DriveID == driveID.String()
	}, "websocket wake source stopped")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketDisabledKeepsPollingOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ socketIOEndpointFetcher, _ driveid.ID, _ socketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	cancel, done := runWatchInBackground(t, eng, SyncDownloadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start when websocket is disabled")
	case <-time.After(100 * time.Millisecond):
	}

	stopWatch(t, cancel, done)
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStarted ||
			event.Type == engineDebugEventWebsocketConnected
	}), "disabled websocket mode must not emit websocket lifecycle events")
}

// Validates: R-2.8.5
func TestRunWatch_MountRootKeepsPollingOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.enableWebsocket = true
	eng.remoteRootItemID = "mount-root-root"
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ socketIOEndpointFetcher, _ driveid.ID, _ socketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	cancel, done := runWatchInBackground(t, eng, SyncDownloadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start for mount-root watch")
	case <-time.After(100 * time.Millisecond):
	}

	stopWatch(t, cancel, done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketFallback && event.Note == "mount_root"
	}, "websocket mount-root fallback")
}

// Validates: R-2.8.3, R-6.8.9, R-6.10.10
func TestRunWatch_CancellationWinsOverFinalObserverExit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()
	require.NoError(t, receiveWithin(t, done, "RunWatch to return after cancellation"),
		"RunWatch should prefer graceful shutdown over all-observers-exited")

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverExited && event.Observer == engineDebugObserverLocal
	}, "local observer exited")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWatchStopped
	}, "watch stopped")
}

// Validates: R-2.1.4
// TestRunWatch_UploadOnly_StartsRemoteObserver verifies that upload-only mode
// still observes remote truth via delta polling.
func TestRunWatch_UploadOnly_StartsRemoteObserver(t *testing.T) {
	t.Parallel()

	var deltaCalls atomic.Int32

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalls.Add(1)
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, WatchOptions{
		PollInterval: 50 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
	})

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	stopWatch(t, cancel, done)

	assert.Positive(t, deltaCalls.Load(), "upload-only watch should still issue delta calls")
	assert.True(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
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

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventMaintenanceTickHandled
	}, "maintenance tick handled")
}

// TestRunWatch_ProcessBatch_EmptyPlan verifies that an empty plan (all
// changes classify to no-op) is handled gracefully.
func TestRunWatch_ProcessBatch_EmptyPlan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	content := "hello"
	contentHash := hashContentQuickXor(t, content)

	// Seed baseline with a synced file.
	seedOutcomes := []actionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-as",
		ItemType:        ItemTypeFile,
		RemoteHash:      contentHash,
		LocalHash:       contentHash,
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")
	writeLocalFile(t, syncRoot, "already-synced.txt", content)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-as",
		Path:     "already-synced.txt",
		ItemType: ItemTypeFile,
		Hash:     contentHash,
		Size:     5,
	}}, ""))

	rt := testWatchRuntime(t, eng)
	applied, err := rt.runSteadyStateReplan(ctx, &watchPipeline{
		bl:   bl,
		mode: SyncBidirectional,
	}, dirtyBatch{})
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

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
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
	rt.sched.runningCount = 1

	err = rt.handleWatchReplanReady(ctx, &watchPipeline{
		bl:   bl,
		mode: SyncBidirectional,
	}, dirtyBatch{})
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

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, SyncDownloadOnly, WatchOptions{
		PollInterval: 1 * time.Hour,
		Debounce:     10 * time.Millisecond,
	})

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	// Wait for the local observer itself: canceling as soon as the remote one
	// starts raced its startup (the remote start's state read saw the
	// cancellation and the runtime stopped before the local observer began).
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	// Create a local file. Download-only still observes it, but upload actions
	// remain suppressed by mode admission.
	writeLocalFile(t, syncRoot, "local-only.txt", "should-be-ignored")

	stopWatch(t, cancel, done)

	bl, err := eng.baseline.Load(context.Background())
	require.NoError(t, err)
	_, found := bl.GetByPath("local-only.txt")
	assert.False(t, found, "download-only watch mode must ignore local-only files created after bootstrap")
	assert.True(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}), "download-only watch must start a local observer")
}

// Validates: R-2.8.22
// TestRunWatch_WatchLimitExhausted_FallsBackToPolling verifies that when the
// local observer returns errLocalWatchUnavailable, the engine does NOT consider
// the observer dead. Instead it falls back to periodic full scanning and
// RunWatch continues until the context is canceled.
func TestRunWatch_WatchLimitExhausted_FallsBackToPolling(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create a subdirectory so the ENOSPC watcher has something to fail on.
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))

	// Inject a watcher factory that returns ENOSPC after the first Add (root).
	watcher := newEnospcWatcher(1)
	eng.localWatcherFactory = func() (fsWatcher, error) {
		return watcher, nil
	}

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, WatchOptions{
		PollInterval: 100 * time.Millisecond, // short for fast test
		Debounce:     10 * time.Millisecond,
	})

	receiveWithin(t, watcher.Failures(), "the local watcher to hit ENOSPC")

	// RunWatch returns nil (clean shutdown), not an "all observers exited" error.
	cancel()
	require.NoError(t, receiveWithin(t, done, "RunWatch to return after cancellation"),
		"RunWatch should return nil on clean shutdown with fallback polling")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_ShutdownStopsRetryAndTrialTimers(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	ctx := t.Context()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   eng.nowFunc().Add(5 * time.Second),
	})
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, testRetryWorkKey("held.txt", "", ActionUpload), SKService())
	require.NoError(t, err)

	_, err = eng.baseline.RecordRetryWorkFailure(ctx, testRetryWorkFailure("retry.txt", "", ActionDownload), func(_ int) time.Duration {
		return 5 * time.Second
	})
	require.NoError(t, err)
	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, quietWatchOptions())

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()
	recorder.waitUntilSeenOrExit(t, done, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventShutdownStarted
	}, "shutdown started")

	clock.Advance(10 * time.Second)
	require.NoError(t, receiveWithin(t, done, "the watch loop to exit once its timers were advanced"))

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventWatchStopped
		},
	}, "local observer start, shutdown, and watch stop should occur in order")
	for _, forbidden := range []engineDebugEventType{
		engineDebugEventRetryTimerFired,
		engineDebugEventRetryHeldReleaseStarted,
		engineDebugEventTrialTimerFired,
		engineDebugEventTrialHeldReleaseStarted,
	} {
		recorder.requireNoEventAfter(t, func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		}, func(event engineDebugEvent) bool {
			return event.Type == forbidden
		}, fmt.Sprintf("%s must not occur after shutdown starts", forbidden))
	}
}

func installTickerCreatedSignal(eng *testEngine, interval time.Duration) <-chan struct{} {
	tickerCreated := make(chan struct{})
	var tickerCreatedOnce atomic.Bool
	observeEngineClock(eng.Engine).onNewTicker = func(nextInterval time.Duration) {
		if nextInterval == interval && tickerCreatedOnce.CompareAndSwap(false, true) {
			close(tickerCreated)
		}
	}

	return tickerCreated
}

func installAfterFuncCreatedSignal(eng *testEngine, delay time.Duration) <-chan struct{} {
	timerCreated := make(chan struct{})
	var timerCreatedOnce atomic.Bool
	observeEngineClock(eng.Engine).onAfterFunc = func(nextDelay time.Duration) {
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
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
				}, "bootstrap-token"), nil
			}
			if refreshStartedOnce.CompareAndSwap(false, true) {
				close(refreshStarted)
			}
			<-refreshReleased
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
				{ID: "late-item", Name: "late.txt", ParentID: "root", DriveID: driveid.New(engineTestDriveID), Size: 10, QuickXorHash: "late-hash"},
			}, "refresh-token"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	watcher := newSignalingWatcher()
	eng.localWatcherFactory = func() (fsWatcher, error) {
		return watcher, nil
	}
	refreshTimerCreated := installAfterFuncCreatedSignal(eng, 15*time.Minute)
	saveObservationCursorForTest(t, eng.baseline, t.Context(), engineTestDriveID, "seed-token")
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(
		t.Context(),
		driveid.New(engineTestDriveID),
		clock.Now().Add(-fullRemoteRefreshInterval+15*time.Minute),
		remoteObservationModeDelta,
	))

	watchCtx, cancel := context.WithCancel(t.Context())
	recorder := attachDebugEventRecorderWithHook(eng, func(event engineDebugEvent) {
		if event.Type == engineDebugEventRemoteRefreshCommitted {
			cancel()
		}
	})

	// The watch runs on watchCtx, which the recorder's hook cancels.
	_, done := runInBackground(t, watchCtx, func(ctx context.Context) error {
		return eng.RunWatch(ctx, SyncUploadOnly, quietWatchOptions())
	})

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")
	receiveWithin(t, watcher.Added(), "local watch setup to add a watcher")
	receiveWithin(t, refreshTimerCreated, "the refresh timer to be created")

	clock.Advance(15 * time.Minute)
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventRemoteRefreshStarted
	}, "full remote refresh started")
	receiveWithin(t, refreshStarted, "the full remote refresh's delta fetch to start")

	close(refreshReleased)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventShutdownStarted
	}, "shutdown started")

	require.NoError(t, receiveWithin(t, done, "the watch loop to exit after shutdown during committed refresh application"))

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventRemoteRefreshStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventRemoteRefreshApplied
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventWatchStopped
		},
	}, "loop-owned refresh application should complete before shutdown drains the watch loop")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventRemoteRefreshDroppedOnShutdown
	}, 0, "applied full remote refresh batches should not later be dropped as pending shutdown work")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_FallbackSleepHonorsCancellation(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))
	recorder := attachDebugEventRecorder(eng)

	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	clock.SetJitter(5 * time.Second)
	installManualClockForTestEngine(eng, clock)

	sleepStarted := make(chan struct{})
	var sleepStartedOnce atomic.Bool
	observeEngineClock(eng.Engine).onSleep = func(time.Duration) {
		if sleepStartedOnce.CompareAndSwap(false, true) {
			close(sleepStarted)
		}
	}

	watcher := newEnospcWatcher(1)
	eng.localWatcherFactory = func() (fsWatcher, error) {
		return watcher, nil
	}
	degradedInterval := localWatchHealthySafetyScanInterval
	tickerCreated := installTickerCreatedSignal(eng, degradedInterval)

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, WatchOptions{
		PollInterval: 1 * time.Second,
		Debounce:     5 * time.Millisecond,
	})

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	receiveWithin(t, tickerCreated, "the fallback ticker to be created")
	clock.Advance(degradedInterval)
	receiveWithin(t, sleepStarted, "the fallback jitter sleep to start")

	// The sleep is a manual clock's: only cancellation can end it.
	stopWatch(t, cancel, done)

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStopped
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

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)

	// Create a plan with mismatched Actions and Deps.
	plan := &actionPlan{
		Actions: []Action{
			{Type: ActionDownload, Path: "file.txt"},
			{Type: ActionDownload, Path: "file2.txt"},
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

	mock := &engineMockClient{}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	dataDir := filepath.Join(tmpDir, "data")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	require.NoError(t, os.MkdirAll(dataDir, 0o750))

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

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
	testWatchRuntime(t, testEng).resources.remoteObs = &remoteObserver{}
	testWatchRuntime(t, testEng).resources.localObs = &localObserver{}

	// First Close should succeed.
	require.NoError(t, eng.Close(t.Context()))

	// Second Close must not panic (idempotency). The baseline DB is already
	// closed. A second Close should still be a clean no-op.
	assert.NotPanics(t, func() {
		assert.NoError(t, eng.Close(t.Context()))
	}, "second Close must not panic")
}
