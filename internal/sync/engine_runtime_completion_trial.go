package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (flow *engineFlow) applyTrialReleaseDecision(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	trialScopeKey ScopeKey,
) ([]*trackedAction, error) {
	if err := flow.releaseScope(ctx, trialScopeKey); err != nil {
		return nil, err
	}
	flow.releaseHeldScope(trialScopeKey)

	return flow.applyCompletionSuccess(ctx, current, r)
}

func (flow *engineFlow) applyTrialExtendDecision(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	trialScopeKey ScopeKey,
) error {
	flow.sched.markFinished(current)
	if err := flow.rehomeBlockedRetryWork(ctx, r, trialScopeKey); err != nil {
		return err
	}
	if err := flow.holdActionUnderScope(ctx, current, r, trialScopeKey); err != nil {
		return err
	}

	return flow.extendScopeTrial(ctx, trialScopeKey, r.RetryAfter)
}

func (flow *engineFlow) applyTrialRearmOrDiscardDecision(
	ctx context.Context,
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
	bl *Baseline,
	trialScopeKey ScopeKey,
) error {
	flow.sched.markFinished(current)
	reclassified, err := flow.applyTrialReclassification(ctx, decision, r, bl)
	if err != nil {
		return err
	}
	if reclassified {
		if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
			return err
		}
	} else {
		persisted, err := flow.applyTrialRetryFallback(ctx, current, decision, r)
		if err != nil {
			return err
		}
		if shouldTransitionTrialFallbackScope(decision) {
			if err := flow.transitionTrialScopeToPersistedBlock(
				ctx,
				trialScopeKey,
				decision.ScopeKey,
				decision.ConditionType,
				r.RetryAfter,
			); err != nil {
				return err
			}
			flow.armFailureTimers(decision, persisted)
			return nil
		}
		if err := flow.rearmOrDiscardScope(ctx, trialScopeKey); err != nil {
			return err
		}
		if err := flow.applyPersistedFailureScopeEffects(ctx, decision, r, persisted); err != nil {
			return err
		}
		flow.armFailureTimers(decision, persisted)
		return nil
	}

	return flow.rearmOrDiscardScope(ctx, trialScopeKey)
}

func (flow *engineFlow) applyTrialRetryFallback(
	ctx context.Context,
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
) (bool, error) {
	if decision.Persistence != persistRetryWork {
		return false, fmt.Errorf("trial retry fallback for %s: missing retry_work persistence", r.Path)
	}
	persisted, err := flow.persistAndHoldFailure(ctx, current, decision, r)
	if err != nil {
		return false, err
	}

	return persisted, nil
}

func shouldTransitionTrialFallbackScope(decision *resultDecision) bool {
	return decision != nil &&
		decision.Class == errclass.ClassScopeBlockingTransient &&
		!decision.ScopeKey.IsZero()
}

func (flow *engineFlow) applyTrialReclassification(
	ctx context.Context,
	decision *resultDecision,
	r *actionCompletion,
	bl *Baseline,
) (bool, error) {
	if handled, err := flow.applyTrialPermissionReclassification(ctx, r, bl); handled {
		return true, err
	}

	if decision.Class == errclass.ClassScopeBlockingTransient && decision.ScopeKey == SKDiskLocal() {
		if err := flow.rehomeBlockedRetryWork(ctx, r, decision.ScopeKey); err != nil {
			return false, err
		}
		return true, flow.applyBlockScope(ctx, scopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ScopeKey.ConditionType(),
		})
	}

	return false, nil
}
