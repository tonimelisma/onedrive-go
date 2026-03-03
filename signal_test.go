package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownContext_FirstSignalCancels(t *testing.T) {
	// Not parallel: sends a real SIGINT to the process. Running in parallel
	// with other signal tests risks interference between signal handlers.

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := shutdownContext(parent, logger)

	// Send SIGINT to ourselves.
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT), "failed to send SIGINT")

	select {
	case <-ctx.Done():
		// Expected: context canceled on first signal.
	case <-time.After(2 * time.Second):
		require.Fail(t, "context not canceled within 2 seconds of SIGINT")
	}

	// Clean up: cancel parent to stop the goroutine.
	cancel()
}

func TestShutdownContext_ParentCancelStopsGoroutine(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := shutdownContext(parent, logger)

	// Cancel parent — derived context should also cancel.
	cancel()

	select {
	case <-ctx.Done():
		// Expected: context canceled when parent is canceled.
	case <-time.After(2 * time.Second):
		require.Fail(t, "context not canceled within 2 seconds of parent cancel")
	}
}

func TestSighupChannel_DeliversSignal(t *testing.T) {
	// Not parallel: sends a real SIGHUP to the process. Running in parallel
	// with other signal tests risks a window where no handler is registered
	// (between signal.Stop and signal.Notify), which terminates the process.

	ch := sighupChannel()
	defer signal.Stop(ch)

	// Send SIGHUP to ourselves.
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP), "failed to send SIGHUP")

	select {
	case sig := <-ch:
		assert.Equal(t, syscall.SIGHUP, sig)
	case <-time.After(2 * time.Second):
		require.Fail(t, "SIGHUP not received within 2 seconds")
	}
}
