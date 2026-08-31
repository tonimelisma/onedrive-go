package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// shutdownContext returns a context that cancels on the first SIGINT/SIGTERM
// and force-exits on the second. The first signal is cooperative: work in
// flight follows its normal shutdown path -- the watch engine seals new
// admission and exits when the runtime settles, and shorter commands unwind
// through their own cleanup. The second signal remains the escape hatch for a
// stuck process.
//
// The returned cancel must be called when the caller is done. It stops the
// signal goroutine and unregisters the handler, which matters because this is
// installed once per command invocation rather than once per process.
func shutdownContext(parent context.Context, logger *slog.Logger) (context.Context, context.CancelFunc) {
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

	return ctx, cancel
}
