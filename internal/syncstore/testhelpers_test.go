package syncstore

// Local test helpers for the syncstore package.
//
// These replicate functionality from the synctest package but are defined here
// to avoid an import cycle: synctest imports syncstore, so syncstore test files
// (which use package syncstore to access unexported symbols) cannot import synctest.

import (
	"context"
	"log/slog"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncrecovery"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// testDriveID is a canonical test drive ID for all syncstore tests.
const testDriveID = "0000000000000001"

// newTestLogger returns a debug-level slog.Logger that writes via t.Log.
// Safe for concurrent use; silently discards writes after the test finishes.
func newTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()

	return slog.New(slog.NewTextHandler(newTestLogWriter(tb), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// testLogWriter adapts testing.TB to io.Writer for slog. Uses a done channel
// to silently discard writes after the test has finished (prevents t.Log races).
type testLogWriter struct {
	t    testing.TB
	done chan struct{}
	once stdsync.Once
}

func newTestLogWriter(tb testing.TB) *testLogWriter {
	tb.Helper()

	w := &testLogWriter{t: tb, done: make(chan struct{})}
	tb.Cleanup(func() { w.once.Do(func() { close(w.done) }) })

	return w
}

type testContextProvider interface {
	Context() context.Context
}

func testContext(tb testing.TB) context.Context {
	tb.Helper()

	if provider, ok := tb.(testContextProvider); ok {
		return provider.Context()
	}

	return context.Background()
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	select {
	case <-w.done:
		return len(p), nil
	default:
	}

	w.t.Helper()
	w.t.Log(string(p))

	return len(p), nil
}

// newTestStore creates a SyncStore backed by a temp directory,
// registering cleanup with t.Cleanup. Does not import synctest to avoid a
// cycle (synctest → syncstore → synctest).
func newTestStore(tb testing.TB) *SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	logger := newTestLogger(tb)

	ctx := testContext(tb)

	mgr, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(ctx), "Close()")
	})

	return mgr
}

func resetInProgressStates(tb testing.TB, mgr *SyncStore, syncRoot string, delayFn func(int) time.Duration) {
	tb.Helper()

	tree, err := synctree.Open(syncRoot)
	require.NoError(tb, err, "synctree.Open(%q)", syncRoot)

	require.NoError(tb, syncrecovery.ResetInProgressStates(testContext(tb), mgr, tree, delayFn, newTestLogger(tb)))
}
