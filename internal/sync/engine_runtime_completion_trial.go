package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// applyTrialReleaseDecision is a successful trial: it releases the next ramp
// batch of the scope's blocked work (R-6.8.24) and then applies the success
// itself. Dependents the success admits are blocked by the still-probationary
// scope and join a later batch. On an error the batch already admitted is
// returned with it, so the caller completes it as shutdown rather than
// leaving it queued.
func (flow *engineFlow) applyTrialReleaseDecision(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	trialScopeKey ScopeKey,
) ([]*trackedAction, error) {
	batch, err := flow.releaseScopeBatch(ctx, trialScopeKey, r.TrialScopeGeneration)
	if err != nil {
		return batch, err
	}

	dependents, err := flow.applyCompletionSuccess(ctx, current, r)
	if err != nil {
		return batch, err
	}

	return append(batch, dependents...), nil
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
	if !reclassified {
		return flow.applyTrialRetryFallback(ctx, current, decision, r, trialScopeKey)
	}
	if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
		return err
	}

	return flow.rearmOrDiscardScope(ctx, trialScopeKey)
}

// applyTrialRetryFallback persists a failed trial as retry work. When the
// failure names a block scope of its own (a scope-blocking answer, or one that
// scope detection turns into a block), the work is persisted blocked under
// that scope and the trial's scope transitions to it in one step, which
// discards the trial's scope if the trial was its only blocked work: checked
// between the two, either order left a block scope without blocked work
// (found by the chaos Graph layer, D-092). Otherwise the trial's scope is
// rearmed or discarded.
func (flow *engineFlow) applyTrialRetryFallback(
	ctx context.Context,
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
	trialScopeKey ScopeKey,
) error {
	if decision.Persistence != persistRetryWork {
		return fmt.Errorf("trial retry fallback for %s: missing retry_work persistence", r.Path)
	}

	persistAs := decision
	target := scopeUpdateResult{}
	if shouldTransitionTrialFallbackScope(decision) {
		target = scopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ConditionType,
			RetryAfter:    r.RetryAfter,
		}
	} else if detected := flow.detectFailureScope(decision, r); detected.Block {
		persistAs = decisionBlockedUnder(decision, detected.ScopeKey)
		target = detected
	}

	persisted, err := flow.persistAndHoldFailure(ctx, current, persistAs, r)
	if err != nil {
		return err
	}
	if target.Block {
		if err := flow.transitionTrialScopeToPersistedBlock(
			ctx, trialScopeKey, target.ScopeKey, target.ConditionType, target.RetryAfter,
		); err != nil {
			return err
		}
	} else if err := flow.rearmOrDiscardScope(ctx, trialScopeKey); err != nil {
		return err
	}
	flow.armFailureTimers(persistAs, persisted)

	return nil
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
