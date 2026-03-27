package syncobserve

import (
	"context"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type watchObserverTestOptions struct {
	Baseline              *synctypes.Baseline
	WriteCoalesceCooldown time.Duration
	CollisionPeers        map[string]map[string]struct{}
	DirNameCache          map[string]map[string][]string
	RecentLocalDeletes    map[string]struct{}
	HashFunc              func(path string) (string, error)
	SafetyTickFunc        func(time.Duration) (<-chan time.Time, func())
}

func newWatchTestObserver(t *testing.T, watcher FsWatcher, opts watchObserverTestOptions) *LocalObserver {
	t.Helper()

	baseline := opts.Baseline
	if baseline == nil {
		baseline = synctest.EmptyBaseline()
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
	if opts.SafetyTickFunc != nil {
		obs.SafetyTickFunc = opts.SafetyTickFunc
	}

	return obs
}
