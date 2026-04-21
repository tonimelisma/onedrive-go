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

type AdmissionDecision struct {
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
func (controller *scopeController) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	decisions := controller.decideAdmission(controller.flow.engine.nowFunc(), ready)
	return controller.applyAdmissionDecisions(ctx, watch, decisions)
}

func (controller *scopeController) decideAdmission(
	now time.Time,
	ready []*TrackedAction,
) []AdmissionDecision {
	decisions := make([]AdmissionDecision, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}

		decision := controller.newAdmissionDecision(ta)
		controller.applyPersistedRetryAdmission(now, ta, &decision)
		controller.applyActiveScopeAdmission(ta, &decision)
		decisions = append(decisions, decision)
	}

	return decisions
}

func (controller *scopeController) newAdmissionDecision(ta *TrackedAction) AdmissionDecision {
	decision := AdmissionDecision{
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

func (controller *scopeController) applyPersistedRetryAdmission(
	now time.Time,
	ta *TrackedAction,
	decision *AdmissionDecision,
) {
	if ta == nil || decision == nil {
		return
	}

	row, ok := controller.flow.retryRowsByKey[decision.RetryWorkKey]
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

func (controller *scopeController) applyActiveScopeAdmission(
	ta *TrackedAction,
	decision *AdmissionDecision,
) {
	if ta == nil || decision == nil || decision.Kind != admissionDispatchNow {
		return
	}

	scopeKey := controller.flow.findBlockingScope(ta)
	if scopeKey.IsZero() {
		return
	}
	if ta.IsTrial && !ta.TrialScopeKey.IsZero() && scopeKey == ta.TrialScopeKey {
		return
	}

	decision.Kind = admissionHoldScope
	decision.ScopeKey = scopeKey
}

func (controller *scopeController) applyAdmissionDecisions(
	ctx context.Context,
	watch *watchRuntime,
	decisions []AdmissionDecision,
) ([]*TrackedAction, error) {
	flow := controller.flow
	var dispatch []*TrackedAction

	for i := range decisions {
		decision := &decisions[i]
		ta := decision.Action
		if ta == nil {
			continue
		}

		if err := controller.applyAdmissionScopeClear(ctx, decision); err != nil {
			return dispatch, err
		}

		switch decision.Kind {
		case admissionDispatchNow:
			flow.markQueued(ta)
			dispatch = append(dispatch, ta)
		case admissionHoldRetry:
			flow.holdAction(ta, heldReasonRetry, ScopeKey{}, decision.NextRetryAt)
		case admissionHoldScope:
			if err := controller.persistHeldScopeDecision(ctx, ta, decision); err != nil {
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

func (controller *scopeController) applyAdmissionScopeClear(
	ctx context.Context,
	decision *AdmissionDecision,
) error {
	if decision == nil || decision.ClearScopeKey.IsZero() {
		return nil
	}

	flow := controller.flow
	if err := controller.clearBlockedRetryWorkForScope(ctx, decision.RetryWorkKey, decision.ClearScopeKey); err != nil {
		return err
	}
	if row, ok := flow.retryRowsByKey[decision.RetryWorkKey]; ok && row.Blocked && row.ScopeKey == decision.ClearScopeKey {
		delete(flow.retryRowsByKey, decision.RetryWorkKey)
	}

	return nil
}

func (controller *scopeController) persistHeldScopeDecision(
	ctx context.Context,
	ta *TrackedAction,
	decision *AdmissionDecision,
) error {
	if ta == nil || decision == nil || decision.ScopeKey.IsZero() {
		return nil
	}

	flow := controller.flow
	row, ok := flow.retryRowsByKey[decision.RetryWorkKey]
	if ok && row.Blocked && row.ScopeKey == decision.ScopeKey {
		return nil
	}

	persisted, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          ta.Action.Path,
		OldPath:       ta.Action.OldPath,
		ActionType:    ta.Action.Type,
		ConditionType: decision.ScopeKey.ConditionType(),
		ScopeKey:      decision.ScopeKey,
		LastError:     "blocked by scope: " + decision.ScopeKey.String(),
		Blocked:       true,
	}, nil)
	if err != nil {
		return fmt.Errorf("persist blocked retry_work for %s under %s: %w", ta.Action.Path, decision.ScopeKey.String(), err)
	}
	if persisted == nil {
		return fmt.Errorf("persist blocked retry_work for %s under %s: missing persisted row", ta.Action.Path, decision.ScopeKey.String())
	}
	flow.retryRowsByKey[decision.RetryWorkKey] = *persisted

	return nil
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown.
func (controller *scopeController) completeSubtree(ready []*TrackedAction) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next := flow.completeDepGraphAction(current.ID, "completeSubtree")
		queue = append(queue, next...)
	}
}

// recordBlockedRetryWork records retry_work for an action that is currently
// blocked by an active scope. Blocked rows have no retry timing until the
// scope is released or trialed.
func (controller *scopeController) recordBlockedRetryWork(ctx context.Context, action *Action, scopeKey ScopeKey) error {
	flow := controller.flow

	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          action.Path,
		OldPath:       action.OldPath,
		ActionType:    action.Type,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		Blocked:       true,
	}, nil)
	if err != nil {
		return fmt.Errorf("record blocked retry_work for %s under %s: %w", action.Path, scopeKey.String(), err)
	}
	if row == nil {
		return fmt.Errorf("record blocked retry_work for %s under %s: missing persisted row", action.Path, scopeKey.String())
	}
	flow.retryRowsByKey[retryWorkKeyForAction(action)] = *row

	return nil
}

func (controller *scopeController) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	flow := controller.flow

	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		HTTPStatus:    r.HTTPStatus,
		Blocked:       true,
	}, nil)
	if err != nil {
		return fmt.Errorf("rehome blocked retry_work for %s under %s: %w", r.Path, scopeKey.String(), err)
	}
	if row == nil {
		return fmt.Errorf("rehome blocked retry_work for %s under %s: missing persisted row", r.Path, scopeKey.String())
	}
	flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row

	return nil
}
