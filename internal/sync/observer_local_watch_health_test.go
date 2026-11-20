package sync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// Validates: R-2.8.22
func TestAddWatchesRecursive_EMFILEReturnsWatchUnavailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o700))

	watcher := newAddErrWatcher(syscall.EMFILE, func(name string) bool {
		return strings.HasSuffix(name, string(filepath.Separator)+"a")
	})

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	require.ErrorIs(t, err, ErrLocalWatchUnavailable)
	cause, ok := WatchUnavailableCauseOf(err)
	require.True(t, ok)
	assert.Equal(t, watchCauseFileLimit, cause)
	assert.Equal(t, []string{root}, watcher.removedPaths(), "the partial watch set is rolled back")
}

// Validates: R-2.8.22
func TestWatch_NewDirectoryEMFILEStopsWatchingForFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	watcher := newAddErrWatcher(syscall.EMFILE, func(name string) bool {
		return filepath.Base(name) == "newdir"
	})
	obs := newWatchTestObserver(t, &watcher.mockFsWatcher, watchObserverTestOptions{})
	obs.watcherFactory = func() (FsWatcher, error) { return watcher, nil }
	// newdir must appear only after setup has walked the tree; created
	// earlier, setup itself meets the limit, closes the watcher, and the
	// Create below would be sent on a closed channel.
	watching := make(chan struct{})
	obs.beforeWatchLoop = func() { close(watching) }

	tree := mustOpenSyncTree(t, dir)
	_, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		return obs.Watch(ctx, tree)
	})

	select {
	case <-watching:
	case err := <-done:
		require.FailNow(t, "watch exited before becoming ready", "returned %v", err)
	case <-time.After(eventWait):
		require.FailNow(t, "timed out waiting for watch setup")
	}

	newDir := filepath.Join(dir, "newdir")
	require.NoError(t, os.Mkdir(newDir, 0o700))
	watcher.events <- fsnotify.Event{Name: newDir, Op: fsnotify.Create}

	// kqueue holds a descriptor per watched directory: once they run out,
	// keeping the watcher starves transfers and SQLite, so watching stops and
	// the runtime falls back to periodic scans.
	err := receiveWithin(t, done, "the watch to stop at the descriptor limit")
	require.ErrorIs(t, err, ErrLocalWatchUnavailable)
	cause, ok := WatchUnavailableCauseOf(err)
	require.True(t, ok)
	assert.Equal(t, watchCauseFileLimit, cause)
}

// ---------------------------------------------------------------------------
// D-077: a renamed or removed directory drops its descendants' watches
// ---------------------------------------------------------------------------

// Validates: R-2.8.14
func TestHandleDelete_DirectoryRemovesDescendantWatchesDeepestFirst(t *testing.T) {
	t.Parallel()

	watcher := newRecordingFsWatcher()
	obs := NewLocalObserver(baselineWith(&BaselineEntry{
		Path: "a", DriveID: driveid.New("d"), ItemID: "a1", ItemType: ItemTypeFolder,
	}), synctest.TestLogger(t), 0)
	// "/sync/a/q" is not registered (its Add failed) but a directory below
	// it is: the subtree walk must still reach it.
	registerWatchedForTest(obs, "/sync", "/sync/a", "/sync/a/q/r", "/sync/a/x", "/sync/a/x/y", "/sync/a/z", "/sync/ab", "/sync/other/a")

	obs.HandleDelete(t.Context(), watcher, mustOpenSyncTree(t, "/sync"), "/sync/a", "a", "a")

	assert.Equal(t, []string{"/sync/a/q/r", "/sync/a/x/y", "/sync/a/x", "/sync/a/z", "/sync/a"}, watcher.getRemovedPaths())
	assert.Equal(t, []string{"/sync", "/sync/ab", "/sync/other/a"}, obs.watchedDirs.paths(),
		"only the removed subtree is forgotten; siblings sharing a name prefix stay")
	assert.Empty(t, obs.watchedDirs.descendants("/sync/a"), "the index holds nothing below the removed directory")
}

// Validates: R-2.8.14
//
// What a deleted path was is decided from the watch set alone: a directory
// the baseline does not know, whose own watch could not be added, still
// drops the watches registered below it, and a deleted file removes none.
func TestHandleDelete_UnknownDirectoryDropsTheWatchesBelowIt(t *testing.T) {
	t.Parallel()

	watcher := newRecordingFsWatcher()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	registerWatchedForTest(obs, "/sync", "/sync/new/x", "/sync/new/x/y", "/sync/news")
	tree := mustOpenSyncTree(t, "/sync")

	obs.HandleDelete(t.Context(), watcher, tree, "/sync/file.txt", "file.txt", "file.txt")
	assert.Empty(t, watcher.getRemovedPaths(), "a deleted file removes no watch")

	obs.HandleDelete(t.Context(), watcher, tree, "/sync/new", "new", "new")

	assert.Equal(t, []string{"/sync/new/x/y", "/sync/new/x"}, watcher.getRemovedPaths(),
		"the watches below the unregistered directory are dropped deepest first, and the directory itself has none to remove")
	assert.Equal(t, []string{"/sync", "/sync/news"}, obs.watchedDirs.paths())
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
	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	batches := localBatches(obs, 64)
	cancel, done := startLocalWatch(t, obs, dir)

	require.NoError(t, os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")))

	// The rename is observed once the moved subtree is re-registered under
	// its new name.
	waitForLocalRow(t, batches, "b/sub")

	writeTestFile(t, dir, "b/sub/new.txt", "after rename")

	patch := waitForLocalPatch(t, batches, func(batch *LocalObservation) bool {
		return slices.ContainsFunc(batch.Rows, func(row LocalStateRow) bool {
			return strings.HasSuffix(row.Path, "/new.txt")
		})
	}, "a row for the file created inside the renamed tree")
	assert.Equal(t, []string{"b/sub/new.txt"}, patchRowPaths([]LocalObservation{patch}),
		"a descendant watch must not keep reporting the directory's old path")
	stopWatch(t, cancel, done)
}

// Validates: R-2.8.14
func TestHandleCreate_ReplacedDirectoryDropsStaleDescendantWatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replaced := filepath.Join(dir, "a")
	require.NoError(t, os.Mkdir(replaced, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(replaced, "fresh"), 0o700))

	watcher := newRecordingFsWatcher()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	// Registrations left by the directory that used to live at "a" and was
	// moved away while watched; its delete event was ignored because "a"
	// exists again.
	stale := filepath.Join(replaced, "old", "deep")
	registerWatchedForTest(obs, dir, replaced, filepath.Join(replaced, "old"), stale, filepath.Join(dir, "a-sibling"))

	obs.handleCreate(t.Context(), mustOpenSyncTree(t, dir), replaced, "a", "a", watcher)

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
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = func(context.Context, string) (string, error) {
		hashCalls.Add(1)
		return "h", nil
	}
	watcher := newRecordingFsWatcher()
	tree := mustOpenSyncTree(t, dir)
	obs.handleCreate(t.Context(), tree, photos, "photos", "photos", watcher)
	require.Equal(t, int64(2), hashCalls.Load(), "the first create scans and hashes the new tree")
	require.Contains(t, obs.watchedDirs.paths(), filepath.Join(photos, "nested"))

	// fsnotify's own Create for the same directory, racing the scan.
	obs.handleCreate(t.Context(), tree, photos, "photos", "photos", watcher)

	assert.Equal(t, int64(2), hashCalls.Load(), "a duplicate Create of the same directory does not re-hash its tree")
	assert.Empty(t, watcher.getRemovedPaths(), "and does not drop its watches")
	assert.Contains(t, obs.watchedDirs.paths(), filepath.Join(photos, "nested"))
}

func registerWatchedForTest(obs *LocalObserver, paths ...string) {
	for _, path := range paths {
		obs.watchedDirs.add(path, nil)
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
		AfterFunc: func(delay time.Duration, fn func()) SyncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
	})
	batches := make(chan LocalObservation, 8)
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, watcher, dir)

	watcher.errs <- fsnotify.ErrEventOverflow

	suspect := receiveWithin(t, batches, "suspect marker")
	assert.Equal(t, LocalTruthSuspect, suspect.Kind)

	// Before the fix nothing rescanned until the five-minute safety scan.
	var timer capturedTimer
	select {
	case timer = <-timers:
	case <-time.After(eventWait):
		require.Fail(t, "an overflow must arm a recovery rescan")
	}
	assert.Equal(t, recoveryScanDelay, timer.delay)

	timer.fn()

	snapshot := receiveWithin(t, batches, "recovery full snapshot")
	require.Equal(t, LocalSnapshot, snapshot.Kind, "the recovery scan replaces local truth with a full snapshot")
	assert.True(t, containsLocalRow(snapshot.Rows, "missed.txt"), "the change the kernel dropped is observed")
	stopWatch(t, cancel, done)
}

// Validates: R-2.8.13
func TestWatch_RecoveryRequestsCoalesceIntoOneArmedScan(t *testing.T) {
	t.Parallel()

	timers := make(chan capturedTimer, 4)
	obs := newWatchTestObserver(t, newMockFsWatcher(), watchObserverTestOptions{
		AfterFunc: func(delay time.Duration, fn func()) SyncTimer {
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

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetLocalObservationBatchChannel(make(chan LocalObservation)) // unbuffered, nobody reading

	obs.TrySendLocalObservationBatch(t.Context(), &LocalObservation{DeletedPaths: []string{"x"}})

	assert.Equal(t, int64(1), obs.DroppedEvents())
	assert.Len(t, obs.recoveryScanRequests, 1, "a dropped batch asks for a recovery rescan")
}

type capturedTimer struct {
	delay time.Duration
	fn    func()
}

type noopSyncTimer struct{}

func (noopSyncTimer) Stop() bool { return true }

func containsLocalRow(rows []LocalStateRow, path string) bool {
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
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.clock = clock
	obs.safetyScanInterval = LocalWatchHealthySafetyScanInterval

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
	for _, want := range []time.Duration{80 * time.Second, 160 * time.Second, LocalWatchHealthySafetyScanInterval, LocalWatchHealthySafetyScanInterval} {
		runArmedRecovery()
		obs.armRecoveryScan()
		assert.Equal(t, want, obs.lastRecoveryDelay)
	}

	// Once the storm has passed the delay is back to its base.
	runArmedRecovery()
	clock.Advance(2 * LocalWatchHealthySafetyScanInterval)
	obs.armRecoveryScan()
	assert.Equal(t, 40*time.Second, obs.lastRecoveryDelay)
}

// Validates: R-2.8.13
func TestCancelRecoveryScan_StaleTimerFiringIsIgnored(t *testing.T) {
	t.Parallel()

	clock := newManualClock(time.Now())
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
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
		AfterFunc: func(delay time.Duration, fn func()) SyncTimer {
			timers <- capturedTimer{delay: delay, fn: fn}
			return noopSyncTimer{}
		},
	})
	obs.watcherFactory = func() (FsWatcher, error) { return watcher, nil }
	batches := make(chan LocalObservation, 8)
	obs.SetLocalObservationBatchChannel(batches)

	cancel, done := startMockWatch(t, obs, &watcher.mockFsWatcher, dir)

	// The kernel dropped the Create for this directory.
	missed := filepath.Join(dir, "created-during-overflow")
	require.NoError(t, os.MkdirAll(filepath.Join(missed, "inner"), 0o700))
	watcher.errs <- fsnotify.ErrEventOverflow

	assert.Equal(t, LocalTruthSuspect, receiveWithin(t, batches, "suspect marker").Kind)
	var timer capturedTimer
	select {
	case timer = <-timers:
	case <-time.After(eventWait):
		require.Fail(t, "an overflow must arm a recovery rescan")
	}
	timer.fn()
	require.Equal(t, LocalSnapshot, receiveWithin(t, batches, "recovery snapshot").Kind)

	// Before the fix the directory stayed unwatched for the whole session.
	assert.Contains(t, watcher.addedPaths(), missed, "the recovery scan watches the directory it found")
	assert.Contains(t, watcher.addedPaths(), filepath.Join(missed, "inner"))
	stopWatch(t, cancel, done)
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
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.activeWatcher = watcher
	tree := mustOpenSyncTree(t, dir)
	rows := []LocalStateRow{{Path: "flaky", ItemType: ItemTypeFolder}}

	obs.rewatchUnwatchedDirectories(tree, rows)
	require.NotContains(t, obs.watchedDirs.paths(), flaky, "the watch limit refused it this time")

	obs.rewatchUnwatchedDirectories(tree, rows)
	assert.Contains(t, obs.watchedDirs.paths(), flaky, "a failed Add is retried at the next full scan")
}
