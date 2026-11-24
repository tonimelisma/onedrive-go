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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.8.22
func TestRunWatch_WatcherCreateEMFILEFallsBackToPeriodicScan(t *testing.T) {
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

	// inotify_init1 fails with EMFILE when max_user_instances is used up.
	// Before the fix this ended local observation for the whole session.
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		return nil, syscall.EMFILE
	}

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started after watcher creation failed with EMFILE")

	cancel()
	require.NoError(t, syncfixture.ReceiveWithin(t, done, "RunWatch to return after cancellation"),
		"a watcher that cannot be created must degrade, not fail the session")
}

func newFallbackTestEngine(t *testing.T) (*testEngine, *syncfixture.DebugEventRecorder, *syncfixture.ManualClock) {
	t.Helper()

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
	installManualClockForTestEngine(eng, clock)

	return eng, recorder, clock
}

// Validates: R-2.8.22
func TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) { return syncfixture.NewEnospcWatcher(1), nil }
	tickerCreated := installTickerCreatedSignal(eng, syncobserve.LocalWatchHealthySafetyScanInterval)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	// Before the fix the first fallback scan came an hour later, and the
	// hashes and recovery scan the stopped watcher had pending were lost.
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackScanned
	}, "immediate fallback scan")

	syncfixture.ReceiveWithin(t, tickerCreated, "fallback scans to run at the safety-scan interval, not hourly")
	clock.Advance(syncobserve.LocalWatchHealthySafetyScanInterval)
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackScanned
	}, "scan at the safety-scan interval")

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.8.22
func TestRunWatch_WatchFallbackRetriesTheWatcher(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	recovered := newSignalingWatcher()
	var factoryCalls atomic.Int64
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		if factoryCalls.Add(1) == 1 {
			return syncfixture.NewEnospcWatcher(1), nil
		}
		return recovered, nil
	}
	retryArmed := installAfterFuncCreatedSignal(eng, syncobserve.LocalWatchRetryInitialDelay)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	syncfixture.ReceiveWithin(t, retryArmed, "a watcher retry to be scheduled")

	// The limit was transient. Before the fix the session stayed degraded.
	clock.Advance(syncobserve.LocalWatchRetryInitialDelay)
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStopped
	}, "fallback ends for the retry")
	syncfixture.ReceiveWithin(t, recovered.Added(), "the retried watcher to be established")
	assert.Equal(t, int64(2), factoryCalls.Load())

	syncfixture.StopWatch(t, cancel, done)
}
