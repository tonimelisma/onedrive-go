package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Mock watcher for unit-testing watchLoop
// ---------------------------------------------------------------------------

// mockFsWatcher implements FsWatcher with injectable channels for testing.
// Add() is a no-op — it silently accepts the real directory paths that
// addWatchesRecursive passes during Watch() startup. This is intentional:
// the backoff/safety-scan tests only exercise watchLoop event handling,
// not the watch-setup walk.
type mockFsWatcher struct {
	events   chan fsnotify.Event
	errs     chan error
	closeOne stdsync.Once // idempotent Close prevents panic on double-close
}

func newMockFsWatcher() *mockFsWatcher {
	return &mockFsWatcher{
		events: make(chan fsnotify.Event, 10),
		errs:   make(chan error, 10),
	}
}

func (m *mockFsWatcher) Add(string) error              { return nil }
func (m *mockFsWatcher) Remove(string) error           { return nil }
func (m *mockFsWatcher) Events() <-chan fsnotify.Event { return m.events }
func (m *mockFsWatcher) Errors() <-chan error          { return m.errs }

func (m *mockFsWatcher) Close() error {
	m.closeOne.Do(func() { close(m.events); close(m.errs) })
	return nil
}

// sleepRecorder captures durations passed to sleepFunc.
type sleepRecorder struct {
	mu       stdsync.Mutex
	calls    []time.Duration
	notifyCh chan struct{} // closed after each call to wake waiters
}

func newSleepRecorder() *sleepRecorder {
	return &sleepRecorder{notifyCh: make(chan struct{})}
}

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, d)
	ch := s.notifyCh
	s.notifyCh = make(chan struct{})
	s.mu.Unlock()

	close(ch) // notify waiters

	return nil
}

// waitForCalls blocks until at least n sleep calls have been recorded.
func (s *sleepRecorder) waitForCalls(t *testing.T, n int) {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		s.mu.Lock()
		count := len(s.calls)
		ch := s.notifyCh
		s.mu.Unlock()

		if count >= n {
			return
		}

		select {
		case <-ch:
		case <-deadline:
			require.Fail(t, "timeout waiting for sleep calls",
				"wanted %d sleep calls, got %d", n, count)
		}
	}
}

func (s *sleepRecorder) getCalls() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]time.Duration, len(s.calls))
	copy(result, s.calls)

	return result
}

// ---------------------------------------------------------------------------
// Watch tests
// ---------------------------------------------------------------------------

func TestWatch_DetectsFileModify(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "existing.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "existing.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: existingHash,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	events := make(chan synctypes.ChangeEvent, 10)
	cancel, done := startLocalWatch(t, obs, dir, events)

	// Modify the file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("modified"), 0o600))

	var ev synctypes.ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for modify event")
	}

	cancel()
	<-done

	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "existing.txt", ev.Path)
	assert.Equal(t, hashContent(t, "modified"), ev.Hash)
}

// Validates: R-2.4
func TestWatch_IgnoresExcludedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 10)
	cancel, done := startLocalWatch(t, obs, dir, events)

	// Create an excluded file — should not produce an event.
	writeTestFile(t, dir, "temp.tmp", "temporary")

	// Then create a valid file — should produce an event.
	time.Sleep(50 * time.Millisecond)
	writeTestFile(t, dir, "valid.txt", "keep")

	var ev synctypes.ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for any event")
	}

	cancel()
	<-done

	assert.Equal(t, "valid.txt", ev.Path, "excluded file should be ignored")
}

func TestWatch_NosyncGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, ".nosync", "")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan synctypes.ChangeEvent, 10)

	err := obs.Watch(t.Context(), mustOpenSyncTree(t, dir), events)
	assert.ErrorIs(t, err, synctypes.ErrNosyncGuard)
}

func TestLocalWatch_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Let the watcher start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Watch should return nil after context cancellation")
	case <-time.After(5 * time.Second):
		require.Fail(t, "Watch did not return after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// watchLoop backoff reset tests (B-189)
// ---------------------------------------------------------------------------

// TestWatchLoop_BackoffResetsOnSafetyScan verifies that after a safety scan
// fires, the error backoff resets to the initial value. Without this reset,
// a previous error escalation would persist, causing unnecessarily long
// waits after unrelated safety scans.
func TestWatchLoop_BackoffResetsOnSafetyScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	recorder := newSleepRecorder()
	mockWatcher := newMockFsWatcher()
	tickCh := make(chan time.Time, 1)

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
		localWatchState: localWatchState{
			PendingTimers: make(map[string]*time.Timer),
			HashRequests:  make(chan HashRequest, HashRequestBufSize),
		},
		SleepFunc: recorder.sleep,
		SafetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return tickCh, func() {}
		},
		WatcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	events := make(chan synctypes.ChangeEvent, 50)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Step 1: Inject a watcher error → watchLoop sleeps with initial backoff (1s).
	mockWatcher.errs <- errors.New("test watcher error 1")
	recorder.waitForCalls(t, 1)

	calls := recorder.getCalls()
	require.Equal(t, retry.WatchLocalPolicy().Base, calls[0],
		"first error should use initial backoff")

	// Step 2: Fire the safety scan tick deterministically (no time.Sleep).
	// The safety scan resets errBackoff to retry.WatchLocalPolicy().Base.
	tickCh <- time.Now()

	// Give watchLoop time to process the tick before we send the next error.
	// Without this, both tickCh and errs could be ready in watchLoop's select
	// simultaneously, and Go would pick non-deterministically — potentially
	// processing the error before the tick resets the backoff.
	time.Sleep(10 * time.Millisecond)

	// Step 3: Inject another watcher error → should use initial backoff (1s),
	// NOT the escalated value (2s), because the safety scan reset it.
	mockWatcher.errs <- errors.New("test watcher error 2")
	recorder.waitForCalls(t, 2)

	calls = recorder.getCalls()
	require.Equal(t, retry.WatchLocalPolicy().Base, calls[1],
		"second error after safety scan should use initial backoff (reset)")

	cancel()
	<-done
}

// TestWatchLoop_BackoffEscalatesWithoutReset verifies that without a safety
// scan, consecutive errors escalate the backoff exponentially.
func TestWatchLoop_BackoffEscalatesWithoutReset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	recorder := newSleepRecorder()
	mockWatcher := newMockFsWatcher()

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
		localWatchState: localWatchState{
			PendingTimers: make(map[string]*time.Timer),
			HashRequests:  make(chan HashRequest, HashRequestBufSize),
		},
		SleepFunc: recorder.sleep,
		SafetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			// Never-firing ticker: no safety scan means backoff keeps escalating.
			return make(chan time.Time), func() {}
		},
		WatcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	events := make(chan synctypes.ChangeEvent, 50)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Inject two consecutive errors.
	mockWatcher.errs <- errors.New("error 1")
	recorder.waitForCalls(t, 1)

	mockWatcher.errs <- errors.New("error 2")
	recorder.waitForCalls(t, 2)

	calls := recorder.getCalls()
	require.Equal(t, retry.WatchLocalPolicy().Base, calls[0], "first error: initial backoff")
	require.Equal(t, retry.WatchLocalPolicy().Base*time.Duration(retry.WatchLocalPolicy().Multiplier), calls[1],
		"second error: escalated backoff (no safety scan to reset)")

	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// Combined fsnotify event tests (B-108, B-117, B-118)
// ---------------------------------------------------------------------------

// TestWatchLoop_ChmodCreateCombinedEvent verifies that a combined Chmod|Create
// fsnotify event (which macOS FSEvents sometimes delivers) is handled as a
// Create, not filtered out as a pure Chmod (B-108). The filter at
// handleFsEvent ignores pure Chmod only; combined events must pass through.
func TestWatchLoop_ChmodCreateCombinedEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "combo.txt", "combined event content")

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Inject a combined Chmod|Create event — macOS FSEvents edge case.
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "combo.txt"),
		Op:   fsnotify.Chmod | fsnotify.Create,
	}

	select {
	case ev := <-events:
		require.Equal(t, synctypes.ChangeCreate, ev.Type, "combined Chmod|Create should be handled as Create")
		require.Equal(t, "combo.txt", ev.Path)
		require.Equal(t, synctypes.SourceLocal, ev.Source)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for combined Chmod|Create event")
	}

	cancel()
	<-done
}

// TestWatchLoop_TransientFileCreateDelete verifies that a transient file
// (created then immediately deleted) does not cause errors when the Remove
// event arrives for a path that was never observed by the watcher (B-117).
// On macOS kqueue, rapid Create+Remove can coalesce into just a Remove event.
// The handler should emit a ChangeDelete with ItemTypeFile (default) and not
// panic or error, even though the path has no baseline entry.
func TestWatchLoop_TransientFileCreateDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Inject a Remove event for a path that was never created (from the
	// watcher's perspective). This simulates macOS kqueue coalescing a
	// rapid Create+Remove into just Remove.
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "transient.txt"),
		Op:   fsnotify.Remove,
	}

	select {
	case ev := <-events:
		require.Equal(t, synctypes.ChangeDelete, ev.Type,
			"transient file delete should produce ChangeDelete")
		require.Equal(t, "transient.txt", ev.Path)
		require.Equal(t, synctypes.ItemTypeFile, ev.ItemType,
			"unknown path defaults to ItemTypeFile")
		require.True(t, ev.IsDeleted)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for transient file delete event")
	}

	cancel()
	<-done
}

// TestWatchLoop_MoveOutOfOrderRenameCreate verifies that out-of-order events
// from a local `mv file.txt dir/file.txt` are handled correctly (B-118).
// The move produces fsnotify Rename (delete at old path) + Create (at new
// path). If events arrive out of order (Create before Rename), both should
// still produce independent ChangeEvents — the planner handles them as
// separate delete + create operations on independent paths.
func TestWatchLoop_MoveOutOfOrderRenameCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create destination directory and the moved file.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dest"), 0o700))
	writeTestFile(t, dir, "dest/moved.txt", "moved content")

	// Baseline has the file at the old path.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "original.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "moved content"),
	})

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		Baseline: baseline,
		Logger:   synctest.TestLogger(t),
		localWatchState: localWatchState{
			PendingTimers: make(map[string]*time.Timer),
			HashRequests:  make(chan HashRequest, HashRequestBufSize),
		},
		SleepFunc: func(_ context.Context, _ time.Duration) error { return nil },
		SafetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		WatcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Inject events in REVERSED order: Create at new path before Rename
	// (delete) at old path. This can happen when fsnotify delivers events
	// out of kernel queue order.
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "dest", "moved.txt"),
		Op:   fsnotify.Create,
	}
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "original.txt"),
		Op:   fsnotify.Rename,
	}

	// Collect both events.
	var collected []synctypes.ChangeEvent
	timeout := time.After(5 * time.Second)

	for len(collected) < 2 {
		select {
		case ev := <-events:
			collected = append(collected, ev)
		case <-timeout:
			require.Fail(t, "timeout", "collected only %d events, want 2", len(collected))
		}
	}

	cancel()
	<-done

	// Find the Create and Delete events (order may vary).
	var createEv, deleteEv *synctypes.ChangeEvent
	for i := range collected {
		if collected[i].Type == synctypes.ChangeCreate {
			createEv = &collected[i]
			continue
		}

		if collected[i].Type == synctypes.ChangeDelete {
			deleteEv = &collected[i]
		}
	}

	require.NotNil(t, createEv, "should have a ChangeCreate for the new path")
	require.Equal(t, "dest/moved.txt", createEv.Path)
	require.Equal(t, synctypes.SourceLocal, createEv.Source)

	require.NotNil(t, deleteEv, "should have a ChangeDelete for the old path")
	require.Equal(t, "original.txt", deleteEv.Path)
	require.True(t, deleteEv.IsDeleted)
	require.Equal(t, synctypes.ItemTypeFile, deleteEv.ItemType,
		"baseline lookup should resolve the item type for the old path")
}
