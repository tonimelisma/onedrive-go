package sync

import "context"

func (flow *engineFlow) applySuccessEffects(ctx context.Context, r *ActionCompletion) {
	flow.succeeded++
	flow.clearRetryWorkOnSuccess(ctx, r)
	if flow.scopeState != nil {
		flow.scopeState.RecordSuccess(r)
	}
}

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
) ([]*TrackedAction, error) {
	flow.markFinished(current)
	flow.applySuccessEffects(ctx, r)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, r.ActionID, "successful action completion")
}

func (flow *engineFlow) drainPublicationSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if current == nil {
		return nil, nil
	}

	flow.markFinished(current)
	flow.succeeded++
	flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, current.ID, "publication action completion")
}
