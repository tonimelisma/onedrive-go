package main

import (
	"context"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestShutdownContext_FirstSignalCancels(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := shutdownContext(parent, logger)

	// Send SIGINT to ourselves.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("failed to send SIGINT: %v", err)
	}

	select {
	case <-ctx.Done():
		// Expected: context canceled on first signal.
	case <-time.After(2 * time.Second):
		t.Fatal("context not canceled within 2 seconds of SIGINT")
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
		t.Fatal("context not canceled within 2 seconds of parent cancel")
	}
}
