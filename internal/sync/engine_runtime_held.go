package sync

import (
	"context"
	"fmt"
	"time"
)

func blockedRetryWorkMessage(scopeKey ScopeKey) string {
	return "blocked by scope: " + scopeKey.String()
}

func (flow *engineFlow) persistBlockedRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	failure *RetryWorkFailure,
) (RetryWorkRow, error) {
	if failure == nil {
		return RetryWorkRow{}, fmt.Errorf("persist blocked retry_work for %s: missing failure", work.Path)
	}
	if failure.ScopeKey.IsZero() {
		return RetryWorkRow{}, fmt.Errorf("persist blocked retry_work for %s: missing scope key", work.Path)
	}

	persisted := &RetryWorkFailure{
		Path:          work.Path,
		OldPath:       work.OldPath,
		ActionType:    work.ActionType,
		ConditionType: failure.ScopeKey.ConditionType(),
		ScopeKey:      failure.ScopeKey,
		LastError:     blockedRetryWorkMessage(failure.ScopeKey),
		HTTPStatus:    failure.HTTPStatus,
		Blocked:       true,
	}
	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, persisted, nil)
	if err != nil {
		return RetryWorkRow{}, fmt.Errorf("persist blocked retry_work for %s under %s: %w", work.Path, failure.ScopeKey.String(), err)
	}
	if row == nil {
		return RetryWorkRow{}, fmt.Errorf(
			"persist blocked retry_work for %s under %s: missing persisted row",
			work.Path,
			failure.ScopeKey.String(),
		)
	}

	flow.retryRowsByKey[work] = *row
	return *row, nil
}

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

	if _, err := flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), &RetryWorkFailure{
		ScopeKey:   scopeKey,
		HTTPStatus: r.HTTPStatus,
	}); err != nil {
		return err
	}
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
	_, err := flow.persistBlockedRetryWork(ctx, retryWorkKeyForAction(action), &RetryWorkFailure{
		ScopeKey: scopeKey,
	})
	return err
}

func (flow *engineFlow) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	_, err := flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), &RetryWorkFailure{
		ScopeKey:   scopeKey,
		HTTPStatus: r.HTTPStatus,
	})
	return err
}
