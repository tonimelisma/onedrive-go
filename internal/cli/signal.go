package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// shutdownContext returns a context that cancels on the first SIGINT/SIGTERM
// and force-exits on the second. The first signal is cooperative: canceling
// the context cancels the work in flight, which unwinds through its own
// cleanup -- the watch engine seals new admission, waits for its canceled
// actions to stop (still committing the outcome of any whose transfer had
// finished), and exits when the runtime settles; shorter commands unwind the
// same way. Work in flight is not waited for to complete. The second signal
// remains the escape hatch for a stuck process.
//
// The returned stop function must be called when the caller is done. It
// cancels the context, stops the signal goroutine and unregisters the handler,
// which matters because this is installed once per command invocation rather
// than once per process (R-6.3.6).
func shutdownContext(parent context.Context, logger *slog.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// released is what lets the goroutine go after a first signal. By then
	// ctx is already canceled, so it says nothing about the command
	// finishing, and parent is context.Background() at the real entry point,
	// so it never fires. Only the caller's stop call ends the wait for a
	// second signal.
	released := make(chan struct{})
	var releaseOnce sync.Once
	stop := func() {
		releaseOnce.Do(func() { close(released) })
		cancel()
	}

	go func() {
		defer signal.Stop(sigCh)

		select {
		case <-sigCh:
			logger.Info("Stopping... (press Ctrl-C again to force quit)")
			cancel()
		case <-ctx.Done():
			return
		}

		// Wait for second signal — force exit.
		select {
		case <-sigCh:
			logger.Warn("Forcing exit")
			os.Exit(1)
		case <-released:
			return
		}
	}()

	return ctx, stop
}
