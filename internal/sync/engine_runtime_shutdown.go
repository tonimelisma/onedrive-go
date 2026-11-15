package sync

import (
	"context"
	"errors"
	"log/slog"
)

// processShutdownCompletion applies the usual completion bookkeeping without
// watch-owned admission/timer side effects, then immediately collapses any
// newly-ready frontier back into shutdown completion instead of dispatch.
func (flow *engineFlow) processShutdownCompletion(
	ctx context.Context,
	completion *actionCompletion,
	bl *Baseline,
) error {
	ctx = contextWithShutdownFreshnessBypass(ctx)
	ready, err := flow.applyRuntimeCompletionStage(ctx, completion, bl)
	flow.completeOutboxAsShutdown(ready)
	return err
}

func (f *engineFlow) completeOutboxAsShutdown(outbox []*trackedAction) {
	for _, ta := range outbox {
		f.completeTrackedActionAsShutdown(ta)
	}
}

func (f *engineFlow) completeTrackedActionAsShutdown(ta *trackedAction) {
	if ta == nil {
		return
	}

	f.sched.markFinished(ta)
	ready := f.completeDepGraphAction(ta.ID, "completeTrackedActionAsShutdown")
	f.completeSubtree(ready)
}

func (flow *engineFlow) logSuppressedShutdownCompletionError(
	completion *actionCompletion,
	err error,
) {
	if completion == nil || err == nil {
		return
	}

	fields := []any{
		slog.String("path", completion.Path),
		slog.String("action_type", completion.ActionType.String()),
		slog.Any("error", err),
	}

	// Only a stopping run gets here. The cancellation's echo is what a clean
	// Ctrl-C does to in-flight bookkeeping, and a repeated sign-in failure is
	// already the run's returned error. Neither is news to the user. Anything
	// else means a finished transfer may not be recorded; the next sync
	// rediscovers it from durable truth (R-2.5.3). A completion whose own
	// store access outlasted its bound is such a record already lost, so the
	// stop's echo, which only says its own write failed too, is no reason to
	// keep that loss at Debug.
	var fatalErr *fatalCompletionError
	lostToTheStore := errors.Is(completion.Err, errStoreAccessTimedOut)
	if (isCancellationEcho(err) && !lostToTheStore) || errors.As(err, &fatalErr) {
		flow.deps.logger.Debug("suppressed action completion error during shutdown", fields...)
		return
	}

	flow.deps.logger.Warn("Could not record the result for "+completion.Path+
		" while stopping; the next sync will check it again", fields...)
}
