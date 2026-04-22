package sync

import (
	"context"
	"fmt"
	"time"
)

func (flow *engineFlow) holdActionFromPersistedRetryState(
	current *TrackedAction,
	work RetryWorkKey,
) error {
	if current == nil {
		return nil
	}

	row, ok := flow.retryRowsByKey[work]
	if !ok {
		return fmt.Errorf("hold action %s from persisted retry state: missing retry_work row", work.Path)
	}
	if row.Blocked {
		flow.holdAction(current, heldReasonScope, row.ScopeKey, time.Time{})
		return nil
	}

	nextRetry := time.Time{}
	if row.NextRetryAt > 0 {
		nextRetry = time.Unix(0, row.NextRetryAt)
	}
	flow.holdAction(current, heldReasonRetry, ScopeKey{}, nextRetry)
	return nil
}

func (flow *engineFlow) holdActionUnderScope(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	if current == nil {
		return nil
	}

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
		return fmt.Errorf("record blocked retry_work for %s under %s: %w", r.Path, scopeKey.String(), err)
	}
	if row == nil {
		return fmt.Errorf("record blocked retry_work for %s under %s: missing persisted row", r.Path, scopeKey.String())
	}
	flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
	flow.holdAction(current, heldReasonScope, scopeKey, time.Time{})
	if watch != nil {
		watch.armHeldTimers()
	}
	return nil
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
	watch *watchRuntime,
) ([]*TrackedAction, error) {
	now := flow.engine.nowFunc()
	var ready []*TrackedAction

	for _, key := range flow.dueRetryKeys(now) {
		if ta := flow.releaseHeldAction(key); ta != nil {
			ta.IsTrial = false
			ta.TrialScopeKey = ScopeKey{}
			ready = append(ready, ta)
		}
	}

	for _, key := range flow.dueTrialKeys(now) {
		held := flow.heldByKey[key]
		if held == nil {
			continue
		}

		ta := flow.releaseHeldAction(key)
		if ta == nil {
			continue
		}
		ta.IsTrial = true
		ta.TrialScopeKey = held.ScopeKey
		ready = append(ready, ta)
	}

	if len(ready) == 0 {
		return nil, nil
	}

	return flow.admitReady(ctx, watch, ready)
}

func (flow *engineFlow) clearBlockedRetryWorkForScope(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	if scopeKey.IsZero() {
		return nil
	}

	if err := flow.engine.baseline.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		return fmt.Errorf("clear blocked retry_work for %s under %s: %w", work.Path, scopeKey.String(), err)
	}
	if row, ok := flow.retryRowsByKey[work]; ok && row.Blocked && row.ScopeKey == scopeKey {
		delete(flow.retryRowsByKey, work)
	}

	return nil
}

// recordBlockedRetryWork records retry_work for an action that is currently
// blocked by an active scope. Blocked rows have no retry timing until the
// scope is released or trialed.
func (flow *engineFlow) recordBlockedRetryWork(ctx context.Context, action *Action, scopeKey ScopeKey) error {
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

func (flow *engineFlow) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
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
