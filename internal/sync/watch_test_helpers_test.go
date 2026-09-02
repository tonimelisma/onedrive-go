package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type watchObserverTestOptions struct {
	Baseline              *Baseline
	WriteCoalesceCooldown time.Duration
	CollisionPeers        map[string]map[string]struct{}
	DirNameCache          map[string]map[string][]string
	RecentLocalDeletes    map[string]struct{}
	HashFunc              func(path string) (string, error)
	AfterFunc             func(time.Duration, func()) syncTimer
	AfterSafetyScan       func()
	TickCh                <-chan time.Time
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

	return obs
}

func startMockWatch(
	t *testing.T,
	obs *localObserver,
	watcher *mockFsWatcher,
	dir string,
	events chan changeEvent,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "watch exited before becoming ready")
	case <-watcher.Added():
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for mock watch setup")
	}

	return cancel, done
}
