package sync

import (
	"os"
	"path/filepath"
	stdsync "sync"
	"syscall"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// estimateDirCount tests
// ---------------------------------------------------------------------------

func TestEstimateDirCount_Empty(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	// +1 for root even with empty baseline.
	assert.Equal(t, 1, obs.EstimateDirCount())
}

func TestEstimateDirCount_WithFolders(t *testing.T) {
	t.Parallel()

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "docs", ItemType: ItemTypeFolder},
		{Path: "docs/sub", ItemType: ItemTypeFolder},
		{Path: "file.txt", ItemType: ItemTypeFile},
	})

	obs := NewLocalObserver(bl, synctest.TestLogger(t), 0)
	// 2 folders + 1 for root = 3.
	assert.Equal(t, 3, obs.EstimateDirCount())
}

// ---------------------------------------------------------------------------
// addWatchesRecursive ENOSPC detection
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestAddWatchesRecursive_ENOSPC_ReturnsWatchLimitExhausted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Create nested dirs to trigger ENOSPC after the root.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o700))

	watcher := newEnospcWatcher(1) // fail after first successful Add (root)

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWatchLimitExhausted,
		"expected ErrWatchLimitExhausted, got: %v", err)
}

// Validates: R-2.1.2
func TestAddWatchesRecursive_ENOSPCRollsBackAddedWatches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o700))

	watcher := newEnospcWatcher(2) // root + a succeed, a/b fails

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	require.Error(t, err)
	require.ErrorIs(t, err, ErrWatchLimitExhausted)
	assert.Equal(t, []string{
		filepath.Join(root, "a"),
		root,
	}, watcher.removedPaths)
	assert.Empty(t, obs.watchedDirs, "rollback should remove newly added watches from observer state")
}

func TestAddWatchesRecursive_NonENOSPC_ContinuesNormally(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o700))

	// Watcher that returns a non-ENOSPC error.
	watcher := &permErrWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: 1,
	}

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	// Non-ENOSPC errors should NOT return ErrWatchLimitExhausted.
	require.NoError(t, err) // walks continue, failures are just logged
	assert.Empty(t, watcher.removed, "ordinary add failures should not trigger rollback")
	assert.Contains(t, obs.watchedDirs, filepath.Clean(root), "successful watches should remain installed")
}

// permErrWatcher returns EPERM after N successful Add calls.
type permErrWatcher struct {
	events    chan fsnotify.Event
	errs      chan error
	addCount  int
	failAfter int
	closeOne  stdsync.Once
	removed   []string
}

func (w *permErrWatcher) Add(string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		return syscall.EPERM
	}

	return nil
}

func (w *permErrWatcher) Remove(name string) error {
	w.removed = append(w.removed, name)
	return nil
}

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
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o700))

	watcher := newEnospcWatcher(1)

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.WatcherFactory = func() (FsWatcher, error) { return watcher, nil }

	events := make(chan ChangeEvent, 256)
	ctx := t.Context()

	err := obs.Watch(ctx, mustOpenSyncTree(t, root), events)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWatchLimitExhausted,
		"Watch should return ErrWatchLimitExhausted, got: %v", err)
}

// ---------------------------------------------------------------------------
// FullScan pre-flight check tests
// ---------------------------------------------------------------------------

func TestFullScan_NonexistentSyncRoot_ReturnsError(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, nonexistent))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSyncRootMissing,
		"FullScan should return ErrSyncRootMissing, got: %v", err)
}
