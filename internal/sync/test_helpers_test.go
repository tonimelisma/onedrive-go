package sync

// test_helpers_test.go provides shared test helper functions for the merged
// sync package's engine, observer, planner, and executor tests.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// testDriveID is the canonical drive ID used by engine tests.
const testDriveID = synctest.TestDriveID

// emptyBaseline returns a Baseline with initialized but empty maps.
func emptyBaseline() *Baseline {
	return NewBaselineForTest(nil)
}

// baselineWith creates a Baseline pre-populated with the given entries.
func baselineWith(entries ...*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// newBaselineForTest seeds a baseline using the store-owned test helper so
// sync tests stay aligned with the current baseline owner.
func newBaselineForTest(entries []*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// actionsOfType filters a flat action list to a single type.
func actionsOfType(actions []Action, actionType ActionType) []Action {
	var result []Action

	for i := range actions {
		if actions[i].Type == actionType {
			result = append(result, actions[i])
		}
	}

	return result
}

// testLogger returns a *slog.Logger wired to t.Log for clean test output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return synctest.TestLogger(t)
}

func newTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return synctest.TestLogger(tb)
}

func newTestStore(tb testing.TB) *SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	ctx := synctest.TestContext(tb)
	mgr, err := NewSyncStore(ctx, dbPath, synctest.TestLogger(tb))
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

func mustOpenSyncTree(t *testing.T, path string) *synctree.Root {
	t.Helper()

	tree, err := synctree.Open(path)
	if err != nil {
		panic(fmt.Sprintf("open sync tree %s: %v", path, err))
	}

	return tree
}

// newTestManager creates a SyncStore backed by a temp directory for use in
// engine tests that need database access (shortcut storage, etc.).
func newTestManager(t *testing.T) *SyncStore {
	t.Helper()

	ctx := synctest.TestContext(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	mgr, err := NewSyncStore(ctx, dbPath, synctest.TestLogger(t))
	require.NoError(t, err, "NewSyncStore(%q)", dbPath)

	t.Cleanup(func() {
		assert.NoError(t, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

// discardLogger returns a logger that writes to nowhere, suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// controllableClock returns a deterministic clock and an advance hook for tests
// that need to move time forward explicitly.
func controllableClock() (nowFunc func() time.Time, advance func(d time.Duration)) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// enospcWatcher returns ENOSPC after N successful Add calls.
// Used by engine tests that verify the ENOSPC fallback-to-polling path.
type enospcWatcher struct {
	events       chan fsnotify.Event
	errs         chan error
	addCount     int
	failAfter    int
	closeOne     stdsync.Once
	failOne      stdsync.Once
	failCh       chan struct{}
	addedPaths   []string
	failedPaths  []string
	removedPaths []string
}

func newEnospcWatcher(failAfter int) *enospcWatcher {
	return &enospcWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: failAfter,
		failCh:    make(chan struct{}),
	}
}

func (w *enospcWatcher) Add(name string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		w.failedPaths = append(w.failedPaths, name)
		w.failOne.Do(func() { close(w.failCh) })
		return syscall.ENOSPC
	}

	w.addedPaths = append(w.addedPaths, name)

	return nil
}

type signalingWatcher struct {
	events   chan fsnotify.Event
	errs     chan error
	addOne   stdsync.Once
	addCh    chan struct{}
	closeOne stdsync.Once
}

func newSignalingWatcher() *signalingWatcher {
	return &signalingWatcher{
		events: make(chan fsnotify.Event, 10),
		errs:   make(chan error, 10),
		addCh:  make(chan struct{}),
	}
}

func (w *signalingWatcher) Add(string) error {
	w.addOne.Do(func() { close(w.addCh) })
	return nil
}

func (w *signalingWatcher) Remove(string) error           { return nil }
func (w *signalingWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *signalingWatcher) Errors() <-chan error          { return w.errs }
func (w *signalingWatcher) Added() <-chan struct{}        { return w.addCh }

func (w *signalingWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })
	return nil
}

func (w *enospcWatcher) Remove(name string) error {
	w.removedPaths = append(w.removedPaths, name)
	return nil
}

func (w *enospcWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *enospcWatcher) Errors() <-chan error          { return w.errs }
func (w *enospcWatcher) Failures() <-chan struct{}     { return w.failCh }

func (w *enospcWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}
