package syncstore

// Local test helpers for the syncstore package.
//
// These replicate functionality from the synctest package but are defined here
// to avoid an import cycle: synctest imports syncstore, so syncstore test files
// (which use package syncstore to access unexported symbols) cannot import synctest.

import (
	"log/slog"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDriveID is a canonical test drive ID for all syncstore tests.
const testDriveID = "0000000000000001"

// newTestLogger returns a debug-level slog.Logger that writes via t.Log.
// Safe for concurrent use; silently discards writes after the test finishes.
func newTestLogger(t testing.TB) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(newTestLogWriter(t), &slog.HandlerOptions{
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

func newTestLogWriter(t testing.TB) *testLogWriter {
	t.Helper()

	w := &testLogWriter{t: t, done: make(chan struct{})}
	t.Cleanup(func() { w.once.Do(func() { close(w.done) }) })

	return w
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
func newTestStore(t testing.TB) *SyncStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := newTestLogger(t)

	mgr, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err, "NewSyncStore(%q)", dbPath)

	t.Cleanup(func() {
		assert.NoError(t, mgr.Close(), "Close()")
	})

	return mgr
}
