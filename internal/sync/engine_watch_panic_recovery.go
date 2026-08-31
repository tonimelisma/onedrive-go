package sync

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Observer names used when reporting a panic, so the log field and the error
// text agree on what stopped.
const (
	observerNameLocal  = "local"
	observerNameRemote = "remote"
)

// recoverObserverPanic converts a panic in a watch observer goroutine into an
// observer error.
//
// A watch observer runs for the lifetime of a mount, so a panic inside one
// would otherwise terminate the process and take every other mount down with
// it. The runtime already knows how to handle an observer that stops: as an
// error it becomes an observer exit, the mount fails, and multisync's
// MountRunner keeps that failure to the one mount.
//
// Register this as the last defer in the goroutine. Deferred calls run in
// reverse order, so it recovers before the observer's channels close and the
// error reaches the loop rather than racing a closed-channel exit.
func recoverObserverPanic(logger *slog.Logger, observer string, errs chan<- error) {
	r := recover()
	if r == nil {
		return
	}

	// The stack is captured here rather than reconstructed from the value
	// because a panic without one is close to undiagnosable in a daemon that
	// has already discarded the failing goroutine.
	logger.Error("panic in watch observer",
		slog.String("observer", observer),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())),
	)

	// Fatal rather than an ordinary observer exit. The runtime tolerates one
	// observer stopping because the conditions it was built for -- watch-limit
	// exhaustion, transient remote failure -- leave the mount degraded but
	// coherent. A panic leaves undefined state instead, and continuing would
	// run the mount half-blind while its status still read healthy. Failing
	// the mount is recoverable; silently observing nothing is not.
	select {
	case errs <- newFatalObserverError(
		fmt.Errorf("sync: panic in %s watch observer: %v", observer, r)):
	default:
		logger.Error("dropped watch observer panic report: error channel full",
			slog.String("observer", observer),
		)
	}
}
