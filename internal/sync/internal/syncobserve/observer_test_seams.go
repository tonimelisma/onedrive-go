package syncobserve

import (
	"context"
	"fmt"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The local observer's test seams: what the engine's tests reach in an
// observer. A ...ForTest function is used only by tests and the sync
// test-helper packages.

// Hooks are the local observer's test seams. The zero value is production
// behavior, and production code passes a Hooks value through without setting
// a field (R-6.10.29).
type Hooks struct {
	// WatcherFactory replaces the fsnotify watcher, so a test can simulate
	// inotify watch exhaustion (ENOSPC) or another platform failure.
	WatcherFactory func() (FsWatcher, error)
	// HashFile replaces the QuickXorHash of a file. It takes the observer's
	// run context because the watch event loop hashes synchronously: a
	// shutdown must not wait out a whole-file read (R-6.3.6).
	HashFile func(ctx context.Context, path string) (string, error)
	// AfterSafetyScan is called after a safety scan's state reset completes.
	AfterSafetyScan func()
	// BeforeWatchLoop is called once Watch's setup is done, before it reads
	// an event.
	BeforeWatchLoop func()
	// WriteCoalesceCooldown, when positive, replaces
	// defaultWriteCoalesceCooldown.
	WriteCoalesceCooldown time.Duration
}

// newWatcher builds the filesystem watcher Watch reads events from.
func (o *LocalObserver) newWatcher() (FsWatcher, error) {
	if o.hooks.WatcherFactory != nil {
		return o.hooks.WatcherFactory()
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	return &fsnotifyWrapper{w: w}, nil
}

// RunSafetyScanForTest runs one safety scan of tree (runSafetyScan).
func (o *LocalObserver) RunSafetyScanForTest(ctx context.Context, tree *synctree.Root) bool {
	return o.runSafetyScan(ctx, tree)
}

// MountBoundaryFlagsForTest is the set of folders flagged as crossed mount
// points; nil when none was ever set.
func (o *LocalObserver) MountBoundaryFlagsForTest() map[string]struct{} {
	flags := o.mountBoundaryFlags.Load()
	if flags == nil {
		return nil
	}

	return *flags
}
