package sync

// test_helpers_test.go provides shared test helper functions used by the
// engine tests in internal/sync. The observer tests (previously defined
// here) have been migrated to internal/syncobserve; these shims remain so
// that the engine tests compile without change.

import (
	"io"
	"log/slog"
	stdsync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// testDriveID is the canonical drive ID used by engine tests.
const testDriveID = synctest.TestDriveID

// emptyBaseline returns a synctypes.Baseline with initialized but empty maps.
func emptyBaseline() *synctypes.Baseline {
	return synctest.EmptyBaseline()
}

// baselineWith creates a synctypes.Baseline pre-populated with the given entries.
func baselineWith(entries ...*synctypes.BaselineEntry) *synctypes.Baseline {
	return synctest.BaselineWith(entries...)
}

// testLogger returns a *slog.Logger wired to t.Log for clean test output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return synctest.TestLogger(t)
}

// newTestManager creates a syncstore.SyncStore backed by a temp directory for use in
// engine tests that need database access (shortcut storage, etc.).
func newTestManager(t *testing.T) *syncstore.SyncStore {
	t.Helper()
	return synctest.NewTestStore(t)
}

// discardLogger returns a logger that writes to nowhere, suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// controllableClock returns a nowFunc and a function to advance the clock.
// The clock starts at a fixed epoch to keep tests deterministic.
//
//nolint:unparam // advance is used by scope_test.go callers; here only nowFunc is needed
func controllableClock() (nowFunc func() time.Time, advance func(d time.Duration)) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) }
}

// enospcWatcher returns ENOSPC after N successful Add calls.
// Used by engine tests that verify the ENOSPC fallback-to-polling path.
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
