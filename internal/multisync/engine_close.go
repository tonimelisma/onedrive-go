package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	// engineCloseSlack is what Engine.Close does besides its store's bounded
	// checkpoint: the stale upload-session sweep and the database close.
	engineCloseSlack = 5 * time.Second
	// engineCloseTimeout bounds the wait for one engine's Close, in watch and
	// one-shot mode alike. Close does not take its context into the session
	// sweep, so the bound is on the wait, not on the call; the store's
	// checkpoint has its own, shorter bound inside it.
	engineCloseTimeout = syncengine.StoreCloseCheckpointTimeout + engineCloseSlack

	// initialStackDumpBytes and maxStackDumpBytes size the buffer for the
	// goroutine dump logged with a stuck sync; a dump that does not fit in
	// the larger one is cut short rather than grown without bound.
	initialStackDumpBytes = 64 << 10
	maxStackDumpBytes     = 4 << 20
)

// ErrSyncStuck means a drive's sync did not stop, or did not close its sync
// state, within its bound after it was told to: a watch runner that did not
// return, or an engine of either mode whose close ran over. One sentinel for
// both, because the condition and its answer are the same: the engine may
// still be writing its store, so nothing may close or replace it, and only
// process exit ends a goroutine that ignores cancellation and releases every
// lock it holds. RunWatch ends with it, a one-shot run reports it in
// RunOnceResult.Stuck, and the command exits 1.
var ErrSyncStuck = errors.New("a drive's sync did not stop when told to")

// closeEngine closes one drive's engine. Cancellation is the normal shutdown
// path and stays at DEBUG; any other close failure means the drive's sync
// state may not have been written cleanly, which is worth one warning.
func (o *Orchestrator) closeEngine(ctx context.Context, mount *mountSpec, engine engineRunner) {
	if engine == nil {
		return
	}
	closeErr := engine.Close(ctx)
	if closeErr == nil {
		return
	}
	if errors.Is(closeErr, context.Canceled) || errors.Is(closeErr, context.DeadlineExceeded) {
		o.logger.Debug("close sync engine during shutdown",
			slog.String("mount_id", mount.id().String()),
			slog.Any("error", closeErr),
		)
		return
	}
	o.logger.Warn(fmt.Sprintf("Couldn't close the sync state for %s cleanly", mount.label()),
		slog.Any("error", closeErr),
	)
}

// closeEngineWithin closes engine on a goroutine of its own and reports
// whether the close finished within engineCloseTimeout. The close runs on a
// context detached from the caller's cancellation: the engine has stopped,
// and its store still deserves a clean close when the run is ending. The
// caller owns the goroutine through this wait; past the bound it reports the
// drive stuck, the run ends with ErrSyncStuck, and process exit ends the
// goroutine.
func (o *Orchestrator) closeEngineWithin(ctx context.Context, mount *mountSpec, engine engineRunner) bool {
	if engine == nil {
		return true
	}

	closed := make(chan struct{})
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineCloseTimeout)
	go func() {
		defer close(closed)
		defer cancel()
		o.closeEngine(closeCtx, mount, engine)
	}()

	return o.waitWithin(closed, engineCloseTimeout)
}

// waitWithin reports whether done closed within bound. It arms the retire
// timer only when done is still open, so a runner or close that already
// finished costs no timer.
func (o *Orchestrator) waitWithin(done <-chan struct{}, bound time.Duration) bool {
	select {
	case <-done:
		return true
	default:
	}
	if bound <= 0 {
		return false
	}

	newTimer := o.retireTimer
	if newTimer == nil {
		newTimer = newOneShotTimer
	}
	expired, stop := newTimer(bound)
	defer stop()

	select {
	case <-done:
		return true
	case <-expired:
		return false
	}
}

// engineCloseStuckDetail says which bound an engine close missed.
func engineCloseStuckDetail() string {
	return "its sync state did not close within " + humanDelay(engineCloseTimeout)
}

// reportStuckSync logs, once, that mount's sync missed a bound, and returns
// the error that ends the run. The Error line carries every goroutine's
// stack, since the stuck goroutine's stack is what says where it hangs.
func (o *Orchestrator) reportStuckSync(mount *mountSpec, detail string) error {
	name := mount.label()
	o.logger.Error(
		fmt.Sprintf("Syncing %s did not stop when told to (%s); onedrive-go is stopping so it can start again cleanly",
			name, detail),
		slog.String("mount_id", mount.id().String()),
		slog.String("stack", allGoroutineStacks()),
	)

	return fmt.Errorf("%s: %w (%s)", name, ErrSyncStuck, detail)
}

// allGoroutineStacks is the stack of every goroutine, cut short at
// maxStackDumpBytes.
func allGoroutineStacks() string {
	buf := make([]byte, initialStackDumpBytes)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= maxStackDumpBytes {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}
