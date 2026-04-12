// Package synctest provides shared test helpers for sync sub-packages.
// This is a regular (non-test) package so it can be imported by test files
// across different packages.
package synctest

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// TestDriveID is a canonical test drive ID used across all sync test files.
const TestDriveID = "0000000000000001"

// TestLogger returns a debug-level logger that writes to t.Log,
// so all activity appears in CI output. Safe for concurrent use and
// silently discards writes after the test finishes (prevents t.Log races).
func TestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()

	return slog.New(slog.NewTextHandler(newTestLogWriter(tb), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// testLogWriter adapts testing.TB to io.Writer for slog. Uses a done channel
// to silently discard writes after the test has finished.
type testLogWriter struct {
	t    testing.TB
	done chan struct{}
	once sync.Once
}

func newTestLogWriter(tb testing.TB) *testLogWriter {
	tb.Helper()

	w := &testLogWriter{t: tb, done: make(chan struct{})}
	tb.Cleanup(func() { w.once.Do(func() { close(w.done) }) })

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

// EmptyBaseline returns a Baseline with empty maps, ready for test use.
func EmptyBaseline() *synctypes.Baseline {
	return synctypes.NewBaselineForTest(nil)
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

// BaselineWith returns a Baseline pre-populated with the given entries.
func BaselineWith(entries ...*synctypes.BaselineEntry) *synctypes.Baseline {
	return synctypes.NewBaselineForTest(entries)
}

// ActionsOfType filters a flat action list to a single type.
// Test-only helper — kept in synctest because no production package needs it.
func ActionsOfType(actions []synctypes.Action, t synctypes.ActionType) []synctypes.Action {
	var result []synctypes.Action

	for i := range actions {
		if actions[i].Type == t {
			result = append(result, actions[i])
		}
	}

	return result
}

// NewTestStore creates a SyncStore backed by a temp directory,
// registering cleanup with t.Cleanup.
func NewTestStore(tb testing.TB) *syncstore.SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	logger := TestLogger(tb)

	ctx := testContext(tb)

	mgr, err := syncstore.NewSyncStore(ctx, dbPath, logger)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(ctx), "Close()")
	})

	return mgr
}
