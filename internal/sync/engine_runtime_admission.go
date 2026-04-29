package sync

import (
	"context"
	"fmt"
	"time"
)

type admissionDecisionKind int

const (
	admissionDispatchNow admissionDecisionKind = iota
	admissionHoldRetry
	admissionHoldScope
)

type admissionDecision struct {
	Action        *TrackedAction
	Kind          admissionDecisionKind
	ScopeKey      ScopeKey
	NextRetryAt   time.Time
	ClearScopeKey ScopeKey
	RetryWorkKey  RetryWorkKey
}

// admitReady applies exact retry/block-scope admission to a ready action set.
// Dependency readiness is already satisfied; admission decides whether the
// action dispatches now or is held as exact work for later release.
func (flow *engineFlow) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	fresh, err := flow.filterFreshReadyActions(ctx, watch, ready)
	if err != nil {
		return nil, err
	}

	decisions := flow.decideAdmission(flow.engine.nowFunc(), fresh)
	return flow.applyAdmissionDecisions(ctx, watch, decisions)
}

func (flow *engineFlow) filterFreshReadyActions(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	fresh := make([]*TrackedAction, 0, len(ready))
	for _, ta := range ready {
		if ta == nil {
			continue
		}

		decision, err := evaluateActionFreshnessFromStore(ctx, flow.engine.baseline, &ta.Action)
		if err != nil {
			return fresh, err
		}
		if decision.Fresh {
			fresh = append(fresh, ta)
			continue
		}

		completionErr := fmt.Errorf("%w: %s", ErrActionPreconditionChanged, decision.Reason)
		completion := actionCompletionFromTrackedAction(ta, nil, completionErr)
		if err := flow.applySupersededCompletion(ctx, watch, ta, &completion, "admission stale action"); err != nil {
			return fresh, err
		}
	}

	return fresh, nil
}

func (flow *engineFlow) decideAdmission(
	now time.Time,
	ready []*TrackedAction,
) []admissionDecision {
	decisions := make([]admissionDecision, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}

		decision := flow.newAdmissionDecision(ta)
		flow.applyPersistedRetryAdmission(now, ta, &decision)
		flow.applyActiveScopeAdmission(ta, &decision)
		decisions = append(decisions, decision)
	}

	return decisions
}

func (flow *engineFlow) newAdmissionDecision(ta *TrackedAction) admissionDecision {
	decision := admissionDecision{
		Action:       ta,
		Kind:         admissionDispatchNow,
		RetryWorkKey: retryWorkKeyForAction(&ta.Action),
	}

	if ta.IsTrial && !ta.TrialScopeKey.IsZero() &&
		!ta.TrialScopeKey.BlocksAction(ta.Action.Path, ta.Action.ThrottleTargetKey(), ta.Action.Type) {
		decision.ClearScopeKey = ta.TrialScopeKey
	}

	return decision
}

func (flow *engineFlow) applyPersistedRetryAdmission(
	now time.Time,
	ta *TrackedAction,
	decision *admissionDecision,
) {
	if ta == nil || decision == nil {
		return
	}

	row, ok := flow.retryRowsByKey[decision.RetryWorkKey]
	if !ok || (!decision.ClearScopeKey.IsZero() && row.ScopeKey == decision.ClearScopeKey) {
		return
	}

	switch {
	case row.Blocked && !row.ScopeKey.IsZero() &&
		(!ta.IsTrial || ta.TrialScopeKey.IsZero() || row.ScopeKey != ta.TrialScopeKey):
		decision.Kind = admissionHoldScope
		decision.ScopeKey = row.ScopeKey
	case row.NextRetryAt > 0:
		nextRetryAt := time.Unix(0, row.NextRetryAt)
		if nextRetryAt.After(now) {
			decision.Kind = admissionHoldRetry
			decision.NextRetryAt = nextRetryAt
		}
	}
}

func (flow *engineFlow) applyActiveScopeAdmission(
	ta *TrackedAction,
	decision *admissionDecision,
) {
	if ta == nil || decision == nil || decision.Kind != admissionDispatchNow {
		return
	}

	scopeKey := flow.findBlockingScope(ta)
	if scopeKey.IsZero() {
		return
	}
	if ta.IsTrial && !ta.TrialScopeKey.IsZero() && scopeKey == ta.TrialScopeKey {
		return
	}

	decision.Kind = admissionHoldScope
	decision.ScopeKey = scopeKey
}

func (flow *engineFlow) applyAdmissionDecisions(
	ctx context.Context,
	watch *watchRuntime,
	decisions []admissionDecision,
) ([]*TrackedAction, error) {
	var dispatch []*TrackedAction

	for i := range decisions {
		decision := &decisions[i]
		ta := decision.Action
		if ta == nil {
			continue
		}

		if err := flow.applyAdmissionScopeClear(ctx, decision); err != nil {
			return dispatch, err
		}

		switch decision.Kind {
		case admissionDispatchNow:
			flow.markQueued(ta)
			dispatch = append(dispatch, ta)
		case admissionHoldRetry:
			flow.holdAction(ta, heldReasonRetry, ScopeKey{}, decision.NextRetryAt)
		case admissionHoldScope:
			if err := flow.persistHeldScopeDecision(ctx, ta, decision); err != nil {
				return dispatch, err
			}
			flow.holdAction(ta, heldReasonScope, decision.ScopeKey, time.Time{})
		}
	}

	if watch != nil {
		watch.armHeldTimers()
	}

	return dispatch, nil
}

func (flow *engineFlow) applyAdmissionScopeClear(
	ctx context.Context,
	decision *admissionDecision,
) error {
	if decision == nil || decision.ClearScopeKey.IsZero() {
		return nil
	}

	if err := flow.clearBlockedRetryWorkForScope(ctx, decision.RetryWorkKey, decision.ClearScopeKey); err != nil {
		return err
	}
	if row, ok := flow.retryRowsByKey[decision.RetryWorkKey]; ok && row.Blocked && row.ScopeKey == decision.ClearScopeKey {
		delete(flow.retryRowsByKey, decision.RetryWorkKey)
	}

	return nil
}

func (flow *engineFlow) persistHeldScopeDecision(
	ctx context.Context,
	ta *TrackedAction,
	decision *admissionDecision,
) error {
	if ta == nil || decision == nil || decision.ScopeKey.IsZero() {
		return nil
	}

	row, ok := flow.retryRowsByKey[decision.RetryWorkKey]
	if ok && row.Blocked && row.ScopeKey == decision.ScopeKey {
		return nil
	}

	work := decision.RetryWorkKey
	if work == (RetryWorkKey{}) {
		work = retryWorkKeyForAction(&ta.Action)
	}
	return flow.persistBlockedRetryWork(ctx, work, decision.ScopeKey)
}
