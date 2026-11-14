package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type watchObserverTestOptions struct {
	Baseline              *Baseline
	WriteCoalesceCooldown time.Duration
	CollisionPeers        map[string]map[string]struct{}
	DirNameCache          map[string]map[string][]string
	RecentLocalDeletes    map[string]struct{}
	HashFunc              func(ctx context.Context, path string) (string, error)
	AfterFunc             func(time.Duration, func()) syncTimer
	AfterSafetyScan       func()
	TickCh                <-chan time.Time
	// DebugEvents is the observer's debug event sink, the one the watch
	// runtime installs (a debugEventRecorder's record in tests).
	DebugEvents func(engineDebugEvent)
}

func newWatchTestObserver(t *testing.T, watcher fsWatcher, opts watchObserverTestOptions) *localObserver {
	t.Helper()

	baseline := opts.Baseline
	if baseline == nil {
		baseline = emptyBaseline()
	}

	obs := newLocalObserver(baseline, synctest.TestLogger(t), 0)

	// One clock value, not a set of independently overridable function fields:
	// a never-firing tick and an instant sleep by default, with the real clock
	// behind anything the test does not pin.
	clock := newObserverTestClock()
	clock.sleep = func(context.Context, time.Duration) error { return nil }
	clock.tickCh = make(chan time.Time)
	obs.clock = clock

	obs.watcherFactory = func() (fsWatcher, error) {
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
// dropped an input (match), failing at once on any change event meanwhile.
// The observer reports the drop on its watch goroutine after anything the
// input emitted, so once the report has arrived an empty events channel
// shows that the input emitted nothing: no window decides the test. A report
// already read by an earlier wait counts.
func requireDroppedWithoutEvent(
	t *testing.T, recorder *debugEventRecorder, events <-chan changeEvent,
	match func(engineDebugEvent) bool, what string,
) {
	t.Helper()

	timer := time.NewTimer(eventWait)
	defer timer.Stop()
	for !recorder.findEvent(match) {
		select {
		case ev := <-events:
			require.FailNow(t, "unexpected event", "got %+v while waiting for %s", ev, what)
		case <-recorder.events:
		case <-timer.C:
			require.FailNow(t, "timed out waiting for "+what)
		}
	}
	requireNoChangeEvent(t, events, what)
}

// requireNoChangeEvent fails when events holds an event. It never waits: the
// caller has a signal that whatever could have been sent was sent before it.
func requireNoChangeEvent(t *testing.T, events <-chan changeEvent, signal string) {
	t.Helper()

	select {
	case ev := <-events:
		require.FailNow(t, "unexpected event", "got %+v before %s", ev, signal)
	default:
	}
}

// collisionSkipOf matches the local observer's report that it skipped the
// entry at path for a case collision.
func collisionSkipOf(path string) func(engineDebugEvent) bool {
	return func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventLocalCaseCollisionSkipped && event.Path == path
	}
}

// startMockWatch runs obs.Watch on dir in the background (runInBackground)
// and returns once the mock watcher has its first directory.
func startMockWatch(
	t *testing.T,
	obs *localObserver,
	watcher *mockFsWatcher,
	dir string,
	events chan changeEvent,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	tree := mustOpenSyncTree(t, dir)
	cancel, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		return obs.Watch(ctx, tree, events)
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
	obs *localObserver,
	watcher *mockFsWatcher,
	dir string,
	events chan changeEvent,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	looping := make(chan struct{})
	obs.beforeWatchLoop = func() { close(looping) }
	cancel, done := startMockWatch(t, obs, watcher, dir, events)
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
