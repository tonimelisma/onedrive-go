package sync

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type watchObserverTestOptions struct {
	Baseline              *Baseline
	WriteCoalesceCooldown time.Duration
	CollisionPeers        map[string]map[string]struct{}
	DirNameCache          map[string]map[namekey.Key][]string
	RecentLocalDeletes    map[string]struct{}
	HashFunc              func(ctx context.Context, path string) (string, error)
	AfterFunc             func(time.Duration, func()) SyncTimer
	AfterSafetyScan       func()
	TickCh                <-chan time.Time
	// DebugEvents is the observer's debug event sink, the one the watch
	// runtime installs (a debugEventRecorder's record in tests).
	DebugEvents func(EngineDebugEvent)
}

func newWatchTestObserver(t *testing.T, watcher FsWatcher, opts watchObserverTestOptions) *LocalObserver {
	t.Helper()

	baseline := opts.Baseline
	if baseline == nil {
		baseline = emptyBaseline()
	}

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)

	// One clock value, not a set of independently overridable function fields:
	// a never-firing tick and an instant sleep by default, with the real clock
	// behind anything the test does not pin.
	clock := newObserverTestClock()
	clock.sleep = func(context.Context, time.Duration) error { return nil }
	clock.tickCh = make(chan time.Time)
	obs.clock = clock

	obs.watcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}

	if opts.WriteCoalesceCooldown != 0 {
		obs.writeCoalesceCooldown = opts.WriteCoalesceCooldown
	}
	if opts.CollisionPeers != nil {
		obs.CollisionPeers = opts.CollisionPeers
	}
	if opts.DirNameCache != nil {
		obs.DirNameCache = opts.DirNameCache
	}
	if opts.RecentLocalDeletes != nil {
		obs.RecentLocalDeletes = opts.RecentLocalDeletes
	}
	if opts.HashFunc != nil {
		obs.hashFunc = opts.HashFunc
	}
	if opts.AfterFunc != nil {
		clock.afterFunc = opts.AfterFunc
	}
	if opts.AfterSafetyScan != nil {
		obs.afterSafetyScan = opts.AfterSafetyScan
	}
	if opts.TickCh != nil {
		clock.tickCh = opts.TickCh
	}
	obs.debugEvents = opts.DebugEvents

	return obs
}

// requireDroppedWithoutEvent waits for the observer's debug report that it
// dropped an input (match), failing at once on any local patch meanwhile.
// The observer reports the drop on its watch goroutine after anything the
// input sent, so once the report has arrived an empty batch channel shows
// that the input sent nothing: no window decides the test. A report already
// read by an earlier wait counts.
func requireDroppedWithoutEvent(
	t *testing.T, recorder *debugEventRecorder, batches <-chan LocalObservation,
	match func(EngineDebugEvent) bool, what string,
) {
	t.Helper()

	timer := time.NewTimer(eventWait)
	defer timer.Stop()
	for !recorder.findEvent(match) {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch {
				require.FailNow(t, "unexpected local patch", "got %+v while waiting for %s", batch, what)
			}
		case <-recorder.events:
		case <-timer.C:
			require.FailNow(t, "timed out waiting for "+what)
		}
	}
	requireNoLocalPatch(t, batches, what)
}

// requireNoLocalPatch fails when batches holds a local patch. It never
// waits: the caller has a signal that whatever could have been sent was sent
// before it. Snapshots and suspect marks are not patches and are skipped.
func requireNoLocalPatch(t *testing.T, batches <-chan LocalObservation, signal string) {
	t.Helper()

	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch {
				require.FailNow(t, "unexpected local patch", "got %+v before %s", batch, signal)
			}
		default:
			return
		}
	}
}

// localBatches installs a buffered local observation channel on obs, the one
// output the watch runtime installs, and returns it.
func localBatches(obs *LocalObserver, size int) chan LocalObservation {
	batches := make(chan LocalObservation, size)
	obs.SetLocalObservationBatchChannel(batches)

	return batches
}

// patchRow returns the row a local patch writes for path.
func patchRow(batch *LocalObservation, path string) (LocalStateRow, bool) {
	if batch.Kind != LocalPatch {
		return LocalStateRow{}, false
	}
	for i := range batch.Rows {
		if batch.Rows[i].Path == path {
			return batch.Rows[i], true
		}
	}

	return LocalStateRow{}, false
}

// patchRemoves reports whether a local patch removes path and its subtree.
func patchRemoves(batch *LocalObservation, path string) bool {
	return batch.Kind == LocalPatch && slices.Contains(batch.DeletedPrefixes, path)
}

// waitForLocalPatch returns the first local patch on batches that match
// accepts, skipping every other observation.
func waitForLocalPatch(
	t *testing.T, batches <-chan LocalObservation, match func(*LocalObservation) bool, what string,
) LocalObservation {
	t.Helper()

	timeout := time.After(eventWait)
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch && match(&batch) {
				return batch
			}
		case <-timeout:
			require.FailNow(t, "timeout waiting for "+what)
			return LocalObservation{}
		}
	}
}

// waitForLocalSnapshot returns the next full scan snapshot on batches,
// skipping every other observation.
func waitForLocalSnapshot(t *testing.T, batches <-chan LocalObservation, what string) LocalObservation {
	t.Helper()

	timeout := time.After(eventWait)
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalSnapshot {
				return batch
			}
		case <-timeout:
			require.FailNow(t, "timeout waiting for "+what)
			return LocalObservation{}
		}
	}
}

// nextLocalPatch returns the next local patch on batches, skipping every
// other observation.
func nextLocalPatch(t *testing.T, batches <-chan LocalObservation, what string) LocalObservation {
	t.Helper()

	return waitForLocalPatch(t, batches, func(*LocalObservation) bool { return true }, what)
}

// nextLocalRow returns the one row of the next local patch on batches: the
// patch an upsert of one path sends.
func nextLocalRow(t *testing.T, batches <-chan LocalObservation, what string) LocalStateRow {
	t.Helper()

	patch := nextLocalPatch(t, batches, what)
	require.Len(t, patch.Rows, 1, "%s: %+v", what, patch)
	require.Empty(t, patch.DeletedPrefixes, "%s: %+v", what, patch)

	return patch.Rows[0]
}

// waitForLocalRow waits for a local patch that writes a row for path and
// returns that row.
func waitForLocalRow(t *testing.T, batches <-chan LocalObservation, path string) LocalStateRow {
	t.Helper()

	var row LocalStateRow
	waitForLocalPatch(t, batches, func(batch *LocalObservation) bool {
		var ok bool
		row, ok = patchRow(batch, path)
		return ok
	}, "a row for "+path)

	return row
}

// waitForLocalRemoval waits for a local patch that removes path.
func waitForLocalRemoval(t *testing.T, batches <-chan LocalObservation, path string) {
	t.Helper()

	waitForLocalPatch(t, batches, func(batch *LocalObservation) bool {
		return patchRemoves(batch, path)
	}, "the removal of "+path)
}

// drainLocalPatches returns every local patch batches holds now, without
// waiting.
func drainLocalPatches(batches <-chan LocalObservation) []LocalObservation {
	var drained []LocalObservation
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch {
				drained = append(drained, batch)
			}
		default:
			return drained
		}
	}
}

// drainPatchRows returns, in order, the rows of every local patch batches
// holds now, without waiting.
func drainPatchRows(batches <-chan LocalObservation) []LocalStateRow {
	var rows []LocalStateRow
	patches := drainLocalPatches(batches)
	for i := range patches {
		rows = append(rows, patches[i].Rows...)
	}

	return rows
}

// patchRowPaths lists the row paths of patches, in order.
func patchRowPaths(patches []LocalObservation) []string {
	var paths []string
	for i := range patches {
		for j := range patches[i].Rows {
			paths = append(paths, patches[i].Rows[j].Path)
		}
	}

	return paths
}

// collisionSkipOf matches the local observer's report that it skipped the
// entry at path for a case collision.
func collisionSkipOf(path string) func(EngineDebugEvent) bool {
	return func(event EngineDebugEvent) bool {
		return event.Type == EngineDebugEventLocalCaseCollisionSkipped && event.Path == path
	}
}

// startMockWatch runs obs.Watch on dir in the background (runInBackground)
// and returns once the mock watcher has its first directory.
func startMockWatch(
	t *testing.T,
	obs *LocalObserver,
	watcher *mockFsWatcher,
	dir string,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	tree := mustOpenSyncTree(t, dir)
	cancel, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		return obs.Watch(ctx, tree)
	})

	select {
	case err := <-done:
		require.FailNow(t, "watch exited before becoming ready", "returned %v", err)
	case <-watcher.Added():
	case <-time.After(eventWait):
		require.FailNow(t, "timed out waiting for mock watch setup")
	}

	return cancel, done
}

// startMockWatchInLoop is startMockWatch that also waits until the watch has
// finished its setup walk and reads events, so a directory the test creates
// afterwards is new to it. One that existed during setup is registered by
// the walk, and its Create event is then a duplicate whose contents are not
// scanned.
func startMockWatchInLoop(
	t *testing.T,
	obs *LocalObserver,
	watcher *mockFsWatcher,
	dir string,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	looping := make(chan struct{})
	obs.beforeWatchLoop = func() { close(looping) }
	cancel, done := startMockWatch(t, obs, watcher, dir)
	receiveWithin(t, looping, "the watch to finish its setup")

	return cancel, done
}

// errRunExitedWithoutReturning is the result of a background run whose
// goroutine ended without run returning: a require inside it failed there.
var errRunExitedWithoutReturning = errors.New("the run's goroutine ended before it returned: a require inside it failed")

// runInBackground runs run in a goroutine the test owns, on a context derived
// from ctx, and returns that context's cancel and the channel run's one result
// arrives on. A cleanup cancels the context and waits, within eventWait, until
// run has returned, so a test that fails early never leaves a watch running
// into TempDir removal, engine cleanup or the next test. The test reads the
// result itself with receiveWithin or stopWatch.
func runInBackground(
	t *testing.T, ctx context.Context, run func(context.Context) error,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		returned := false
		defer func() {
			if !returned {
				done <- errRunExitedWithoutReturning
			}
		}()
		err := run(runCtx)
		returned = true
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(eventWait):
			assert.Fail(t, "a background watch did not return after its cleanup canceled it")
		}
	})

	return cancel, done
}

// runWithin is a test's own call of a watch or loop: it runs run on a context
// derived from ctx and returns run's result, failing the test naming what it
// waited for when run has not returned within eventWait. Its return is an
// event like any other, so it is bounded like one.
func runWithin(t *testing.T, ctx context.Context, what string, run func(context.Context) error) error {
	t.Helper()

	_, done := runInBackground(t, ctx, run)

	return receiveWithin(t, done, what)
}

// quietWatchOptions are watch options for a test that drives every change
// itself: the remote poll an hour apart, so it never fires, and a short
// debounce.
func quietWatchOptions() WatchOptions {
	return WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond}
}

// runWatchInBackground runs eng.RunWatch in mode with opts in the background
// (runInBackground).
func runWatchInBackground(t *testing.T, eng *testEngine, mode SyncMode, opts WatchOptions) (context.CancelFunc, <-chan error) {
	t.Helper()

	return runInBackground(t, t.Context(), func(ctx context.Context) error {
		return eng.RunWatch(ctx, mode, opts)
	})
}

// receiveWithin returns what ch delivers, and fails the test naming what it
// waited for when nothing comes within eventWait: a run that never returns
// fails in a minute with a useful line instead of at go test's deadline.
func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(eventWait):
		require.FailNow(t, "timed out waiting for "+what)
		var none T
		return none
	}
}

// stopWatch cancels a background watch and requires that it returns nil
// within eventWait.
func stopWatch(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	cancel()
	require.NoError(t, receiveWithin(t, done, "the watch to return after cancellation"))
}
