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
	SafetyTickFunc        func(time.Duration) (<-chan time.Time, func())
}

func newWatchTestObserver(t *testing.T, watcher FsWatcher, opts watchObserverTestOptions) *LocalObserver {
	t.Helper()

	baseline := opts.Baseline
	if baseline == nil {
		baseline = emptyBaseline()
	}

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.SleepFunc = func(_ context.Context, _ time.Duration) error { return nil }
	obs.SafetyTickFunc = func(time.Duration) (<-chan time.Time, func()) {
		return make(chan time.Time), func() {}
	}
	obs.WatcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}

	if opts.WriteCoalesceCooldown != 0 {
		obs.WriteCoalesceCooldown = opts.WriteCoalesceCooldown
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
		obs.HashFunc = opts.HashFunc
	}
	if opts.AfterFunc != nil {
		obs.AfterFunc = opts.AfterFunc
	}
	if opts.AfterSafetyScan != nil {
		obs.AfterSafetyScan = opts.AfterSafetyScan
	}
	if opts.SafetyTickFunc != nil {
		obs.SafetyTickFunc = opts.SafetyTickFunc
	}

	return obs
}

func startMockWatch(
	t *testing.T,
	obs *LocalObserver,
	watcher *mockFsWatcher,
	dir string,
	events chan ChangeEvent,
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
