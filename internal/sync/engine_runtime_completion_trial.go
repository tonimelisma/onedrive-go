package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (flow *engineFlow) applyTrialReleaseDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	trialScopeKey ScopeKey,
) ([]*TrackedAction, error) {
	if err := flow.releaseScope(ctx, watch, trialScopeKey); err != nil {
		return nil, err
	}
	flow.releaseHeldScope(trialScopeKey)

	return flow.applyCompletionSuccess(ctx, watch, current, r)
}

func (flow *engineFlow) applyTrialExtendDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	trialScopeKey ScopeKey,
) error {
	flow.markFinished(current)
	if err := flow.rehomeBlockedRetryWork(ctx, r, trialScopeKey); err != nil {
		return err
	}
	if err := flow.holdActionUnderScope(ctx, watch, current, r, trialScopeKey); err != nil {
		return err
	}

	return flow.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
}

func (flow *engineFlow) applyTrialRearmOrDiscardDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
	trialScopeKey ScopeKey,
) error {
	flow.markFinished(current)
	reclassified, err := flow.applyTrialReclassification(ctx, watch, decision, r, bl)
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
				watch,
				trialScopeKey,
				decision.ScopeKey,
				decision.ConditionType,
				r.RetryAfter,
			); err != nil {
				return err
			}
			flow.armFailureTimers(watch, decision, persisted)
			return nil
		}
		if err := flow.rearmOrDiscardScope(ctx, watch, trialScopeKey); err != nil {
			return err
		}
		if err := flow.applyPersistedFailureScopeEffects(ctx, watch, decision, r, persisted); err != nil {
			return err
		}
		flow.armFailureTimers(watch, decision, persisted)
		return nil
	}

	return flow.rearmOrDiscardScope(ctx, watch, trialScopeKey)
}

func (flow *engineFlow) applyTrialRetryFallback(
	ctx context.Context,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
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

func shouldTransitionTrialFallbackScope(decision *ResultDecision) bool {
	return decision != nil &&
		decision.Class == errclass.ClassBlockScopeingTransient &&
		!decision.ScopeKey.IsZero()
}

func (flow *engineFlow) applyTrialReclassification(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	if handled, err := flow.applyTrialPermissionReclassification(ctx, watch, r, bl); handled {
		return true, err
	}

	if decision.Class == errclass.ClassBlockScopeingTransient && decision.ScopeKey == SKDiskLocal() {
		if err := flow.rehomeBlockedRetryWork(ctx, r, decision.ScopeKey); err != nil {
			return false, err
		}
		return true, flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ScopeKey.ConditionType(),
		})
	}

	return false, nil
}
