package sync

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Live incident, 2026-11-01: the watch runtime's loop sat inside a ten-minute
// replan, its batch channel filled, and every dropped batch asked for a
// recovery scan. Each recovery scan's snapshot was dropped too and asked for
// the next one: six full scans of 11,000 entries, at doubling delays, that
// delivered nothing. A scan whose result cannot be handed over is deferred,
// not run.
//
// Validates: R-2.8.21
func TestWatch_RecoveryScanDefersWhileRuntimeHasNotTakenObservations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "missed.txt", "the runtime has not seen this yet")

	var scans atomic.Int32
	timers := make(chan capturedTimer, 8)
	watcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, watcher, watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) syncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
		AfterSafetyScan: func() { scans.Add(1) },
	})
	// One undelivered batch is all it takes: the runtime has not drained
	// what it was already sent.
	batches := make(chan localObservationBatch, 1)
	batches <- localObservationBatch{deletedPaths: []string{"queued"}}
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, watcher, dir, make(chan changeEvent, 10))
	defer func() {
		cancel()
		<-done
	}()

	watcher.errs <- fsnotify.ErrEventOverflow

	first := waitForCapturedTimer(t, timers, "an overflow arms a recovery rescan")
	assert.Equal(t, recoveryScanDelay, first.delay)
	first.fn()

	// Before the fix the scan ran here, its snapshot was dropped, and the
	// drop re-armed the next scan at twice the delay.
	second := waitForCapturedTimer(t, timers, "a deferred recovery scan re-arms itself")
	assert.Equal(t, int32(0), scans.Load(), "no scan runs while its result would be dropped")
	assert.Equal(t, recoveryScanDelay, second.delay, "a deferral is not a storm: the pace does not double")

	// The runtime drains its channel; the next due scan runs and lands.
	<-batches
	second.fn()

	snapshot := receiveBatch(t, batches, "recovery full snapshot once the runtime has room")
	require.True(t, snapshot.fullSnapshot)
	assert.True(t, containsLocalRow(snapshot.rows, "missed.txt"))
	assert.Equal(t, int32(1), scans.Load())
}

// The periodic safety scan is deferred the same way, and leaves a recovery
// scan armed so the tree is still rescanned soon after the runtime drains,
// rather than at the next safety interval.
//
// Validates: R-2.8.21
func TestWatch_SafetyTickDefersWhileRuntimeHasNotTakenObservations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "missed.txt", "the runtime has not seen this yet")

	var scans atomic.Int32
	timers := make(chan capturedTimer, 8)
	tick := make(chan time.Time)
	watcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, watcher, watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) syncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
		AfterSafetyScan: func() { scans.Add(1) },
		TickCh:          tick,
	})
	batches := make(chan localObservationBatch, 1)
	batches <- localObservationBatch{deletedPaths: []string{"queued"}}
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, watcher, dir, make(chan changeEvent, 10))
	defer func() {
		cancel()
		<-done
	}()

	tick <- time.Now()

	recovery := waitForCapturedTimer(t, timers, "a deferred safety scan arms a recovery scan")
	assert.Equal(t, int32(0), scans.Load(), "the safety scan does not run while its result would be dropped")
	assert.Equal(t, recoveryScanDelay, recovery.delay)

	<-batches
	recovery.fn()

	snapshot := receiveBatch(t, batches, "full snapshot once the runtime has room")
	require.True(t, snapshot.fullSnapshot)
	assert.True(t, containsLocalRow(snapshot.rows, "missed.txt"))
	assert.Equal(t, int32(1), scans.Load())
}

func waitForCapturedTimer(t *testing.T, timers <-chan capturedTimer, what string) capturedTimer {
	t.Helper()

	select {
	case timer := <-timers:
		return timer
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for a timer", what)
		return capturedTimer{}
	}
}
