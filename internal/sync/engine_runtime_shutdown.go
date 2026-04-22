package sync

import (
	"context"
	"log/slog"
)

// processShutdownCompletion applies the usual completion bookkeeping without
// watch-owned admission/timer side effects, then immediately collapses any
// newly-ready frontier back into shutdown completion instead of dispatch.
func (flow *engineFlow) processShutdownCompletion(
	ctx context.Context,
	completion *ActionCompletion,
	bl *Baseline,
) error {
	ready, err := flow.applyRuntimeCompletionStage(ctx, nil, completion, bl)
	flow.completeOutboxAsShutdown(ready)
	return err
}

func (f *engineFlow) completeOutboxAsShutdown(outbox []*TrackedAction) {
	for _, ta := range outbox {
		f.completeTrackedActionAsShutdown(ta)
	}
}

func (f *engineFlow) completeTrackedActionAsShutdown(ta *TrackedAction) {
	if ta == nil {
		return
	}

	f.markFinished(ta)
	ready := f.completeDepGraphAction(ta.ID, "completeTrackedActionAsShutdown")
	f.completeSubtree(ready)
}

func (flow *engineFlow) logSuppressedShutdownCompletionError(
	completion *ActionCompletion,
	err error,
) {
	if completion == nil || err == nil {
		return
	}

	flow.engine.logger.Warn("suppressed action completion error during shutdown",
		slog.String("path", completion.Path),
		slog.String("action_type", completion.ActionType.String()),
		slog.String("error", err.Error()),
	)
}
