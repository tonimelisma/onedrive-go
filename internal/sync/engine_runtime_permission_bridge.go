package sync

import (
	"context"
	"fmt"
)

func (flow *engineFlow) maybeHandlePermissionOutcome(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	if decision.PermissionFlow == permissionFlowNone {
		return false, nil
	}

	permOutcome, handled := flow.decidePermissionOutcome(ctx, decision, r, bl)
	if !handled || !permOutcome.Matched {
		return false, nil
	}

	if _, err := flow.applyPermissionOutcome(ctx, watch, decision.PermissionFlow, &permOutcome); err != nil {
		return true, flow.failAfterControlStateError(current, err)
	}
	if err := flow.applyPermissionOutcomeHold(ctx, watch, current, r, &permOutcome); err != nil {
		return true, flow.failAfterControlStateError(current, err)
	}
	if watch != nil {
		watch.armHeldTimers()
	}
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) applyPermissionOutcomeHold(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	permOutcome *PermissionOutcome,
) error {
	if permOutcome == nil {
		return nil
	}

	switch {
	case permOutcome.Kind == permissionOutcomeNone:
		if blocking := flow.findBlockingScope(current); !blocking.IsZero() {
			return flow.holdActionUnderScope(ctx, watch, current, r, blocking)
		}
		return nil
	case !permOutcome.ScopeKey.IsZero():
		return flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
	default:
		return flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
	}
}

// decidePermissionOutcome is the shared engine-owned bridge between
// permission evidence gathering and pure policy. Normal completions and trial
// reclassification both call this helper so the engine makes one consistent
// evidence -> outcome decision before any persistence occurs.
func (flow *engineFlow) decidePermissionOutcome(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) (PermissionOutcome, bool) {
	switch decision.PermissionFlow {
	case permissionFlowNone:
		return PermissionOutcome{}, false
	case permissionFlowRemote403:
		if bl == nil || !flow.engine.permHandler.HasPermChecker() {
			return PermissionOutcome{}, false
		}
		permEvidence := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType)
		return DecidePermissionOutcome(r, permEvidence), true
	case permissionFlowLocalPermission:
		permEvidence := flow.engine.permHandler.handleLocalPermission(ctx, r)
		return DecidePermissionOutcome(r, permEvidence), true
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
}
