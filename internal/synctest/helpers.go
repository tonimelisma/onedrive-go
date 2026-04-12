// Package synctest provides shared test helpers for sync sub-packages.
// This is a regular (non-test) package so it can be imported by test files
// across different packages.
package synctest

import (
	"context"
	"log/slog"
	"sync"
	"testing"
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

type testContextProvider interface {
	Context() context.Context
}

// TestContext returns the best available test-scoped context.
func TestContext(tb testing.TB) context.Context {
	tb.Helper()

	if provider, ok := tb.(testContextProvider); ok {
		return provider.Context()
	}

	return context.Background()
}
