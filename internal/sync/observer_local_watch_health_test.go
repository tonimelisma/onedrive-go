package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// addErrWatcher is a mock watcher whose Add fails with addErr for any path
// for which failPath returns true. It records adds and removes.
type addErrWatcher struct {
	mockFsWatcher
	mu       stdsync.Mutex
	failPath func(string) bool
	addErr   error
	added    []string
	removed  []string
}

func newAddErrWatcher(addErr error, failPath func(string) bool) *addErrWatcher {
	return &addErrWatcher{
		mockFsWatcher: mockFsWatcher{
			events: make(chan fsnotify.Event, 10),
			errs:   make(chan error, 10),
			addCh:  make(chan struct{}),
		},
		failPath: failPath,
		addErr:   addErr,
	}
}

func (w *addErrWatcher) Add(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.failPath != nil && w.failPath(name) {
		return w.addErr
	}
	w.added = append(w.added, name)
	w.addOne.Do(func() { close(w.addCh) })

	return nil
}

func (w *addErrWatcher) Remove(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removed = append(w.removed, name)

	return nil
}

func (w *addErrWatcher) removedPaths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]string(nil), w.removed...)
}

// ---------------------------------------------------------------------------
// D-078: resource-limit failures fall back instead of ending local sync
// ---------------------------------------------------------------------------

// Validates: R-2.8.12
func TestAddWatchesRecursive_EMFILEReturnsWatchUnavailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o700))

	watcher := newAddErrWatcher(syscall.EMFILE, func(name string) bool {
		return strings.HasSuffix(name, string(filepath.Separator)+"a")
	})

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	require.ErrorIs(t, err, errLocalWatchUnavailable)
	cause, ok := watchUnavailableCauseOf(err)
	require.True(t, ok)
	assert.Equal(t, watchCauseFileLimit, cause)
	assert.Equal(t, []string{root}, watcher.removedPaths(), "the partial watch set is rolled back")
}

// Validates: R-2.8.12
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
	eng.localWatcherFactory = func() (fsWatcher, error) {
		return nil, syscall.EMFILE
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started after watcher creation failed with EMFILE")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a watcher that cannot be created must degrade, not fail the session")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}
}

// Validates: R-2.8.12
func TestWatch_NewDirectoryEMFILEStopsWatchingForFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	watcher := newAddErrWatcher(syscall.EMFILE, func(name string) bool {
		return filepath.Base(name) == "newdir"
	})
	obs := newWatchTestObserver(t, &watcher.mockFsWatcher, watchObserverTestOptions{})
	obs.watcherFactory = func() (fsWatcher, error) { return watcher, nil }
	// newdir must appear only after setup has walked the tree; created
	// earlier, setup itself meets the limit, closes the watcher, and the
	// Create below would be sent on a closed channel.
	watching := make(chan struct{})
	obs.beforeWatchLoop = func() { close(watching) }

	events := make(chan changeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	select {
	case <-watching:
	case err := <-done:
		require.NoError(t, err, "watch exited before becoming ready")
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for watch setup")
	}

	newDir := filepath.Join(dir, "newdir")
	require.NoError(t, os.Mkdir(newDir, 0o700))
	watcher.events <- fsnotify.Event{Name: newDir, Op: fsnotify.Create}

	select {
	case err := <-done:
		// kqueue holds a descriptor per watched directory: once they run out,
		// keeping the watcher starves transfers and SQLite, so watching stops
		// and the runtime falls back to periodic scans.
		require.ErrorIs(t, err, errLocalWatchUnavailable)
		cause, ok := watchUnavailableCauseOf(err)
		require.True(t, ok)
		assert.Equal(t, watchCauseFileLimit, cause)
	case <-time.After(5 * time.Second):
		require.Fail(t, "watch kept running after the descriptor limit was reached")
	}
}

// ---------------------------------------------------------------------------
// D-077: a renamed or removed directory drops its descendants' watches
// ---------------------------------------------------------------------------

// Validates: R-2.8.14
func TestHandleDelete_DirectoryRemovesDescendantWatchesDeepestFirst(t *testing.T) {
	t.Parallel()

	watcher := newRecordingFsWatcher()
	obs := newLocalObserver(baselineWith(&BaselineEntry{
		Path: "a", DriveID: driveid.New("d"), ItemID: "a1", ItemType: ItemTypeFolder,
	}), synctest.TestLogger(t), 0)
	// "/sync/a/q" is not registered (its Add failed) but a directory below
	// it is: the subtree walk must still reach it.
	registerWatchedForTest(obs, "/sync", "/sync/a", "/sync/a/q/r", "/sync/a/x", "/sync/a/x/y", "/sync/a/z", "/sync/ab", "/sync/other/a")

	events := make(chan changeEvent, 10)
	obs.HandleDelete(t.Context(), watcher, mustOpenSyncTree(t, "/sync"), "/sync/a", "a", "a", events)

	assert.Equal(t, []string{"/sync/a/q/r", "/sync/a/x/y", "/sync/a/x", "/sync/a/z", "/sync/a"}, watcher.getRemovedPaths())
	assert.Equal(t, []string{"/sync", "/sync/ab", "/sync/other/a"}, obs.watchedDirs.paths(),
		"only the removed subtree is forgotten; siblings sharing a name prefix stay")
	assert.Empty(t, obs.watchedDirs.descendants("/sync/a"), "the index holds nothing below the removed directory")
}

// Validates: R-2.8.14
func TestWatch_RenamedDirectoryDescendantEventsUseNewPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "sub"), 0o700))

	baseline := baselineWith(
		&BaselineEntry{Path: "a", DriveID: driveid.New("d"), ItemID: "a1", ItemType: ItemTypeFolder},
		&BaselineEntry{Path: "a/sub", DriveID: driveid.New("d"), ItemID: "s1", ItemType: ItemTypeFolder},
	)
	obs := newLocalObserver(baseline, synctest.TestLogger(t), 0)
	events := make(chan changeEvent, 64)
	cancel, done := startLocalWatch(t, obs, dir, events)
	defer func() {
		cancel()
		<-done
	}()

	require.NoError(t, os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")))

	// The rename is observed once the moved subtree is re-registered under
	// its new name.
	waitForLocalEvent(t, events, func(ev changeEvent) bool {
		return ev.Type == changeCreate && ev.Path == "b/sub"
	}, "re-registration of b/sub")

	writeTestFile(t, dir, "b/sub/new.txt", "after rename")

	ev := waitForLocalEvent(t, events, func(ev changeEvent) bool {
		return strings.HasSuffix(ev.Path, "/new.txt")
	}, "event for the file created inside the renamed tree")
	assert.Equal(t, "b/sub/new.txt", ev.Path,
		"a descendant watch must not keep reporting the directory's old path")
}

// Validates: R-2.8.14
func TestHandleCreate_ReplacedDirectoryDropsStaleDescendantWatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replaced := filepath.Join(dir, "a")
	require.NoError(t, os.Mkdir(replaced, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(replaced, "fresh"), 0o700))

	watcher := newRecordingFsWatcher()
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	// Registrations left by the directory that used to live at "a" and was
	// moved away while watched; its delete event was ignored because "a"
	// exists again.
	stale := filepath.Join(replaced, "old", "deep")
	registerWatchedForTest(obs, dir, replaced, filepath.Join(replaced, "old"), stale, filepath.Join(dir, "a-sibling"))

	obs.handleCreate(t.Context(), mustOpenSyncTree(t, dir), replaced, "a", "a", watcher, make(chan changeEvent, 10))

	assert.Equal(t, []string{stale, filepath.Join(replaced, "old")}, watcher.getRemovedPaths(),
		"stale descendant watches of the replaced directory are dropped deepest first")
	assert.NotContains(t, obs.watchedDirs.paths(), stale)
	assert.Contains(t, obs.watchedDirs.paths(), replaced, "the new directory is watched")
	assert.Contains(t, obs.watchedDirs.paths(), filepath.Join(replaced, "fresh"), "the new tree is registered")
	assert.Contains(t, obs.watchedDirs.paths(), filepath.Join(dir, "a-sibling"))
}

// Validates: R-2.8.14
func TestHandleCreate_DuplicateDirectoryCreateDoesNotRescanItsTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "photos/a.jpg", "image")
	writeTestFile(t, dir, "photos/nested/b.jpg", "image")
	photos := filepath.Join(dir, "photos")

	var hashCalls atomic.Int64
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = func(string) (string, error) {
		hashCalls.Add(1)
		return "h", nil
	}
	watcher := newRecordingFsWatcher()
	tree := mustOpenSyncTree(t, dir)
	events := make(chan changeEvent, 16)

	obs.handleCreate(t.Context(), tree, photos, "photos", "photos", watcher, events)
	require.Equal(t, int64(2), hashCalls.Load(), "the first create scans and hashes the new tree")
	require.Contains(t, obs.watchedDirs.paths(), filepath.Join(photos, "nested"))

	// fsnotify's own Create for the same directory, racing the scan.
	obs.handleCreate(t.Context(), tree, photos, "photos", "photos", watcher, events)

	assert.Equal(t, int64(2), hashCalls.Load(), "a duplicate Create of the same directory does not re-hash its tree")
	assert.Empty(t, watcher.getRemovedPaths(), "and does not drop its watches")
	assert.Contains(t, obs.watchedDirs.paths(), filepath.Join(photos, "nested"))
}

func registerWatchedForTest(obs *localObserver, paths ...string) {
	for _, path := range paths {
		obs.watchedDirs.add(path, nil)
	}
}

func waitForLocalEvent(t *testing.T, events <-chan changeEvent, match func(changeEvent) bool, what string) changeEvent {
	t.Helper()

	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if match(ev) {
				return ev
			}
		case <-timeout:
			require.Fail(t, "timeout waiting for "+what)
			return changeEvent{}
		}
	}
}

// ---------------------------------------------------------------------------
// Recovery scan: known-missed changes trigger a prompt full rescan
// ---------------------------------------------------------------------------

// Validates: R-2.8.13
func TestWatch_EventOverflowRunsRecoveryScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "missed.txt", "the kernel dropped this create")

	timers := make(chan capturedTimer, 4)
	watcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, watcher, watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) syncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
	})
	batches := make(chan localObservationBatch, 8)
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, watcher, dir, make(chan changeEvent, 10))
	defer func() {
		cancel()
		<-done
	}()

	watcher.errs <- fsnotify.ErrEventOverflow

	suspect := receiveBatch(t, batches, "suspect marker")
	assert.True(t, suspect.markSuspect)

	// Before the fix nothing rescanned until the five-minute safety scan.
	var timer capturedTimer
	select {
	case timer = <-timers:
	case <-time.After(5 * time.Second):
		require.Fail(t, "an overflow must arm a recovery rescan")
	}
	assert.Equal(t, recoveryScanDelay, timer.delay)

	timer.fn()

	snapshot := receiveBatch(t, batches, "recovery full snapshot")
	require.True(t, snapshot.fullSnapshot, "the recovery scan replaces local truth with a full snapshot")
	assert.True(t, containsLocalRow(snapshot.rows, "missed.txt"), "the change the kernel dropped is observed")
}

// Validates: R-2.8.13
func TestWatch_RecoveryRequestsCoalesceIntoOneArmedScan(t *testing.T) {
	t.Parallel()

	timers := make(chan capturedTimer, 4)
	obs := newWatchTestObserver(t, newMockFsWatcher(), watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) syncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
	})

	obs.armRecoveryScan()
	obs.armRecoveryScan()
	obs.requestRecoveryScan()
	obs.requestRecoveryScan() // coalesces with the pending request

	assert.Len(t, timers, 1, "a storm of missed-change signals arms one scan")
	assert.Len(t, obs.recoveryScanRequests, 1)

	obs.cancelRecoveryScan()
	assert.Nil(t, obs.recoveryScanTimer, "a full scan disarms the pending recovery scan")
}

// Validates: R-2.8.13
func TestTrySendLocalObservationBatch_DropRequestsRecoveryScan(t *testing.T) {
	t.Parallel()

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetLocalObservationBatchChannel(make(chan localObservationBatch)) // unbuffered, nobody reading

	obs.TrySendLocalObservationBatch(t.Context(), localObservationBatch{deletedPaths: []string{"x"}})

	assert.Equal(t, int64(1), obs.DroppedEvents())
	assert.Len(t, obs.recoveryScanRequests, 1, "a dropped batch asks for a recovery rescan")
}

type capturedTimer struct {
	delay time.Duration
	fn    func()
}

type noopSyncTimer struct{}

func (noopSyncTimer) Stop() bool { return true }

func receiveBatch(t *testing.T, batches <-chan localObservationBatch, what string) localObservationBatch {
	t.Helper()

	select {
	case batch := <-batches:
		return batch
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for "+what)
		return localObservationBatch{}
	}
}

func containsLocalRow(rows []localStateRow, path string) bool {
	for i := range rows {
		if rows[i].Path == path {
			return true
		}
	}

	return false
}

// Validates: R-2.8.14
func TestWatchRegistry_SubtreeLookupFollowsTheIndexAndUnlinksEmptyBranches(t *testing.T) {
	t.Parallel()

	var reg watchRegistry
	reg.add("/r/a/b", nil)
	reg.add("/r/a/b/c", nil)
	reg.add("/r/z", nil)

	assert.Equal(t, []string{"/r/a/b/c", "/r/a/b"}, reg.descendants("/r/a"),
		"registered dirs below an unregistered intermediate are reachable")

	reg.remove("/r/a/b/c")
	reg.remove("/r/a/b")
	assert.Empty(t, reg.descendants("/r"+"/a"))
	assert.NotContains(t, reg.children["/r"], "/r/a", "an empty branch is unlinked from its parent")
	assert.Equal(t, []string{"/r/z"}, reg.descendants("/r"))
}

// Validates: R-2.8.13
func TestRecoveryScanDelay_ScalesWithScanDurationAndBacksOffUnderStorm(t *testing.T) {
	t.Parallel()

	clock := newManualClock(time.Now())
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.clock = clock
	obs.safetyScanInterval = localWatchHealthySafetyScanInterval

	// A tree whose full scan takes 20 s is not rescanned 10 s apart.
	obs.lastFullScanDuration = 20 * time.Second
	obs.armRecoveryScan()
	assert.True(t, clock.HasPendingTimerAt(clock.Now().Add(40*time.Second)), "delay is twice the last scan's duration")

	// The storm continues: every request soon after a recovery scan doubles
	// the delay, up to the safety-scan interval.
	runArmedRecovery := func() {
		t.Helper()
		clock.Advance(obs.lastRecoveryDelay)
		gen := <-obs.recoveryScanDue
		require.True(t, obs.takeRecoveryScanDue(gen))
		obs.finishRecoveryScan()
	}
	for _, want := range []time.Duration{80 * time.Second, 160 * time.Second, localWatchHealthySafetyScanInterval, localWatchHealthySafetyScanInterval} {
		runArmedRecovery()
		obs.armRecoveryScan()
		assert.Equal(t, want, obs.lastRecoveryDelay)
	}

	// Once the storm has passed the delay is back to its base.
	runArmedRecovery()
	clock.Advance(2 * localWatchHealthySafetyScanInterval)
	obs.armRecoveryScan()
	assert.Equal(t, 40*time.Second, obs.lastRecoveryDelay)
}

// Validates: R-2.8.13
func TestCancelRecoveryScan_StaleTimerFiringIsIgnored(t *testing.T) {
	t.Parallel()

	clock := newManualClock(time.Now())
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.clock = clock
	obs.armRecoveryScan()
	clock.mu.Lock()
	staleFire := clock.timers[len(clock.timers)-1].fn
	clock.mu.Unlock()

	// A full scan disarms the recovery scan; a real timer can still fire
	// after Stop and deliver its token.
	obs.cancelRecoveryScan()
	staleFire()

	gen := <-obs.recoveryScanDue
	assert.False(t, obs.takeRecoveryScanDue(gen), "a token from a canceled timer does not trigger a scan")
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

func runWatchInBackground(t *testing.T, eng *testEngine) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	return cancel, done
}

func stopWatch(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return after cancellation")
	}
}

// Validates: R-2.8.12
func TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	eng.localWatcherFactory = func() (fsWatcher, error) { return newEnospcWatcher(1), nil }
	tickerCreated := installTickerCreatedSignal(eng, localWatchHealthySafetyScanInterval)

	cancel, done := runWatchInBackground(t, eng)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	// Before the fix the first fallback scan came an hour later, and the
	// hashes and recovery scan the stopped watcher had pending were lost.
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackScanned
	}, "immediate fallback scan")

	waitForSignal(t, tickerCreated, "fallback scans run at the safety-scan interval, not hourly")
	clock.Advance(localWatchHealthySafetyScanInterval)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackScanned
	}, "scan at the safety-scan interval")

	stopWatch(t, cancel, done)
}

// Validates: R-2.8.12
func TestRunWatch_WatchFallbackRetriesTheWatcher(t *testing.T) {
	t.Parallel()

	eng, recorder, clock := newFallbackTestEngine(t)
	recovered := newSignalingWatcher()
	var factoryCalls atomic.Int64
	eng.localWatcherFactory = func() (fsWatcher, error) {
		if factoryCalls.Add(1) == 1 {
			return newEnospcWatcher(1), nil
		}
		return recovered, nil
	}
	retryArmed := installAfterFuncCreatedSignal(eng, localWatchRetryInitialDelay)

	cancel, done := runWatchInBackground(t, eng)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	waitForSignal(t, retryArmed, "a watcher retry is scheduled")

	// The limit was transient. Before the fix the session stayed degraded.
	clock.Advance(localWatchRetryInitialDelay)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStopped
	}, "fallback ends for the retry")
	waitForSignal(t, recovered.Added(), "the retried watcher is established")
	assert.Equal(t, int64(2), factoryCalls.Load())

	stopWatch(t, cancel, done)
}

func (w *addErrWatcher) addedPaths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]string(nil), w.added...)
}

// Validates: R-2.8.13
func TestWatch_DirectoryCreatedDuringOverflowIsWatchedAfterRecoveryScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	watcher := newAddErrWatcher(nil, nil)
	timers := make(chan capturedTimer, 4)
	obs := newWatchTestObserver(t, &watcher.mockFsWatcher, watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) syncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
	})
	obs.watcherFactory = func() (fsWatcher, error) { return watcher, nil }
	batches := make(chan localObservationBatch, 8)
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, &watcher.mockFsWatcher, dir, make(chan changeEvent, 10))
	defer func() {
		cancel()
		<-done
	}()

	// The kernel dropped the Create for this directory.
	missed := filepath.Join(dir, "created-during-overflow")
	require.NoError(t, os.MkdirAll(filepath.Join(missed, "inner"), 0o700))
	watcher.errs <- fsnotify.ErrEventOverflow

	assert.True(t, receiveBatch(t, batches, "suspect marker").markSuspect)
	var timer capturedTimer
	select {
	case timer = <-timers:
	case <-time.After(5 * time.Second):
		require.Fail(t, "an overflow must arm a recovery rescan")
	}
	timer.fn()
	require.True(t, receiveBatch(t, batches, "recovery snapshot").fullSnapshot)

	// Before the fix the directory stayed unwatched for the whole session.
	assert.Contains(t, watcher.addedPaths(), missed, "the recovery scan watches the directory it found")
	assert.Contains(t, watcher.addedPaths(), filepath.Join(missed, "inner"))
}

// Validates: R-2.8.13
func TestRewatchUnwatchedDirectories_FailedAddIsRetriedAtNextScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flaky := filepath.Join(dir, "flaky")
	require.NoError(t, os.Mkdir(flaky, 0o700))
	failures := 1
	watcher := newAddErrWatcher(syscall.ENOSPC, func(name string) bool {
		if name == flaky && failures > 0 {
			failures--
			return true
		}
		return false
	})
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.activeWatcher = watcher
	tree := mustOpenSyncTree(t, dir)
	rows := []localStateRow{{Path: "flaky", ItemType: ItemTypeFolder}}

	obs.rewatchUnwatchedDirectories(tree, rows)
	require.NotContains(t, obs.watchedDirs.paths(), flaky, "the watch limit refused it this time")

	obs.rewatchUnwatchedDirectories(tree, rows)
	assert.Contains(t, obs.watchedDirs.paths(), flaky, "a failed Add is retried at the next full scan")
}
