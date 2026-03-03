package sync

import (
	"errors"
	"os"
	"path/filepath"
	stdsync "sync"
	"syscall"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// estimateDirCount tests
// ---------------------------------------------------------------------------

func TestEstimateDirCount_Empty(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	// +1 for root even with empty baseline.
	assert.Equal(t, 1, obs.estimateDirCount())
}

func TestEstimateDirCount_WithFolders(t *testing.T) {
	t.Parallel()

	bl := emptyBaseline()
	bl.ByPath["docs"] = &BaselineEntry{
		Path:     "docs",
		ItemType: ItemTypeFolder,
	}
	bl.ByPath["docs/sub"] = &BaselineEntry{
		Path:     "docs/sub",
		ItemType: ItemTypeFolder,
	}
	bl.ByPath["file.txt"] = &BaselineEntry{
		Path:     "file.txt",
		ItemType: ItemTypeFile,
	}

	obs := NewLocalObserver(bl, testLogger(t), 0)
	// 2 folders + 1 for root = 3.
	assert.Equal(t, 3, obs.estimateDirCount())
}

// ---------------------------------------------------------------------------
// addWatchesRecursive ENOSPC detection
// ---------------------------------------------------------------------------

// enospcWatcher returns ENOSPC after N successful Add calls.
type enospcWatcher struct {
	events      chan fsnotify.Event
	errs        chan error
	addCount    int
	failAfter   int
	closeOne    stdsync.Once
	addedPaths  []string
	failedPaths []string
}

func newEnospcWatcher(failAfter int) *enospcWatcher {
	return &enospcWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: failAfter,
	}
}

func (w *enospcWatcher) Add(name string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		w.failedPaths = append(w.failedPaths, name)
		return syscall.ENOSPC
	}

	w.addedPaths = append(w.addedPaths, name)

	return nil
}

func (w *enospcWatcher) Remove(string) error           { return nil }
func (w *enospcWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *enospcWatcher) Errors() <-chan error          { return w.errs }

func (w *enospcWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}

func TestAddWatchesRecursive_ENOSPC_ReturnsWatchLimitExhausted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Create nested dirs to trigger ENOSPC after the root.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))

	watcher := newEnospcWatcher(1) // fail after first successful Add (root)

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	err := obs.addWatchesRecursive(watcher, root)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWatchLimitExhausted),
		"expected ErrWatchLimitExhausted, got: %v", err)
}

func TestAddWatchesRecursive_NonENOSPC_ContinuesNormally(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))

	// Watcher that returns a non-ENOSPC error.
	watcher := &permErrWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: 1,
	}

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	err := obs.addWatchesRecursive(watcher, root)

	// Non-ENOSPC errors should NOT return ErrWatchLimitExhausted.
	assert.NoError(t, err) // walks continue, failures are just logged
}

// permErrWatcher returns EPERM after N successful Add calls.
type permErrWatcher struct {
	events    chan fsnotify.Event
	errs      chan error
	addCount  int
	failAfter int
	closeOne  stdsync.Once
}

func (w *permErrWatcher) Add(string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		return syscall.EPERM
	}

	return nil
}

func (w *permErrWatcher) Remove(string) error           { return nil }
func (w *permErrWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *permErrWatcher) Errors() <-chan error          { return w.errs }

func (w *permErrWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}

// ---------------------------------------------------------------------------
// Watch returns ErrWatchLimitExhausted
// ---------------------------------------------------------------------------

func TestWatch_ENOSPC_ReturnsWatchLimitExhausted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))

	watcher := newEnospcWatcher(1)

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	obs.watcherFactory = func() (FsWatcher, error) { return watcher, nil }

	events := make(chan ChangeEvent, 256)
	ctx := t.Context()

	err := obs.Watch(ctx, root, events)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWatchLimitExhausted),
		"Watch should return ErrWatchLimitExhausted, got: %v", err)
}

// ---------------------------------------------------------------------------
// FullScan pre-flight check tests
// ---------------------------------------------------------------------------

func TestFullScan_NonexistentSyncRoot_ReturnsError(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := obs.FullScan(t.Context(), nonexistent)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSyncRootMissing),
		"FullScan should return ErrSyncRootMissing, got: %v", err)
}
