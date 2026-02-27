package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// shutdownContext returns a context that cancels on the first SIGINT/SIGTERM
// and force-exits on the second. This gives the engine time to drain in-flight
// actions on first signal, while allowing the user to force-quit if something
// hangs.
func shutdownContext(parent context.Context, logger *slog.Logger) context.Context {
	ctx, cancel := context.WithCancel(parent)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(sigCh)

		select {
		case sig := <-sigCh:
			logger.Info("received signal, initiating graceful shutdown",
				slog.String("signal", sig.String()),
			)
			cancel()
		case <-ctx.Done():
			return
		}

		// Wait for second signal — force exit.
		select {
		case sig := <-sigCh:
			logger.Warn("received second signal, forcing exit",
				slog.String("signal", sig.String()),
			)
			os.Exit(1)
		case <-parent.Done():
			return
		}
	}()

	return ctx
}
