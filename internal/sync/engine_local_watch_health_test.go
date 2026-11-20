package sync

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.8.22
func TestRunWatch_WatcherCreateEMFILEFallsBackToPeriodicScan(t *testing.T) {
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

	// inotify_init1 fails with EMFILE when max_user_instances is used up.
	// Before the fix this ended local observation for the whole session.
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return nil, syscall.EMFILE
	}

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackStarted
	}, "fallback started after watcher creation failed with EMFILE")

	cancel()
	require.NoError(t, receiveWithin(t, done, "RunWatch to return after cancellation"),
		"a watcher that cannot be created must degrade, not fail the session")
}

func newFallbackTestEngine(t *testing.T) (*testEngine, *debugEventRecorder, *manualClock) {
	t.Helper()

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
	installManualClockForTestEngine(eng, clock)

	return eng, recorder, clock
}

// Validates: R-2.8.22
func TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	eng.localWatcherFactory = func() (FsWatcher, error) { return newEnospcWatcher(1), nil }
	tickerCreated := installTickerCreatedSignal(eng, LocalWatchHealthySafetyScanInterval)

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackStarted
	}, "fallback started")
	// Before the fix the first fallback scan came an hour later, and the
	// hashes and recovery scan the stopped watcher had pending were lost.
	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackScanned
	}, "immediate fallback scan")

	receiveWithin(t, tickerCreated, "fallback scans to run at the safety-scan interval, not hourly")
	clock.Advance(LocalWatchHealthySafetyScanInterval)
	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackScanned
	}, "scan at the safety-scan interval")

	stopWatch(t, cancel, done)
}

// Validates: R-2.8.22
func TestRunWatch_WatchFallbackRetriesTheWatcher(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	recovered := newSignalingWatcher()
	var factoryCalls atomic.Int64
	eng.localWatcherFactory = func() (FsWatcher, error) {
		if factoryCalls.Add(1) == 1 {
			return newEnospcWatcher(1), nil
		}
		return recovered, nil
	}
	retryArmed := installAfterFuncCreatedSignal(eng, LocalWatchRetryInitialDelay)

	cancel, done := runWatchInBackground(t, eng, SyncUploadOnly, quietWatchOptions())

	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackStarted
	}, "fallback started")
	receiveWithin(t, retryArmed, "a watcher retry to be scheduled")

	// The limit was transient. Before the fix the session stayed degraded.
	clock.Advance(LocalWatchRetryInitialDelay)
	recorder.waitForEvent(t, func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventObserverFallbackStopped
	}, "fallback ends for the retry")
	receiveWithin(t, recovered.Added(), "the retried watcher to be established")
	assert.Equal(t, int64(2), factoryCalls.Load())

	stopWatch(t, cancel, done)
}
