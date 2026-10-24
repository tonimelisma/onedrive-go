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
		slog.String("error", err.Error()),
	}

	// Cancellation is what a clean Ctrl-C does to in-flight bookkeeping, and
	// a repeated sign-in failure is already the run's returned error. Neither
	// is news to the user. Anything else means a finished transfer may not be
	// recorded; the next sync rediscovers it from durable truth (R-2.5.3).
	var fatalErr *fatalCompletionError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &fatalErr) {
		flow.deps.logger.Debug("suppressed action completion error during shutdown", fields...)
		return
	}

	flow.deps.logger.Warn("Could not record the result for "+completion.Path+
		" while stopping; the next sync will check it again", fields...)
}
