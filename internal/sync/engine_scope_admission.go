package sync

import (
	"context"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// admitReady applies watch-mode trial interception and scope admission to a
// ready action set, returning the actions that should enter the watch loop's
// outbox. It is the single admission path used by both newly-planned actions
// and newly-ready dependents from result processing.
func (controller *scopeController) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) []*TrackedAction {
	flow := controller.flow

	var dispatch []*TrackedAction

	for _, ta := range ready {
		if ta.IsTrial {
			if ta.TrialScopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ThrottleTargetKey(), ta.Action.Type) {
				dispatch = append(dispatch, ta)
			} else {
				controller.clearBlockedRetryWorkForScope(ctx, retryWorkKeyForAction(&ta.Action), ta.TrialScopeKey)

				if key := watch.findBlockingScope(ta); key.IsZero() {
					flow.setDispatch(ctx, &ta.Action)
					dispatch = append(dispatch, ta)
				}
				if watch != nil {
					watch.armTrialTimer()
				}
			}

			continue
		}

		if watch != nil {
			if key := watch.findBlockingScope(ta); !key.IsZero() {
				controller.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		flow.setDispatch(ctx, &ta.Action)
		dispatch = append(dispatch, ta)
	}

	return dispatch
}

// cascadeRecordAndComplete records a scope-blocked action and all its
// transitive dependents as blocked retry_work, completing each in the graph.
func (controller *scopeController) cascadeRecordAndComplete(
	ctx context.Context,
	ta *TrackedAction,
	scopeKey ScopeKey,
) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := []*TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		controller.recordBlockedRetryWork(ctx, &current.Action, scopeKey)
		ready := flow.completeDepGraphAction(current.ID, "cascadeRecordAndComplete")
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph.
func (controller *scopeController) cascadeFailAndComplete(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
	r *ActionCompletion,
) {
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

		controller.recordCascadeRetryWork(ctx, watch, &current.Action, r)
		next := flow.completeDepGraphAction(current.ID, "cascadeFailAndComplete")
		queue = append(queue, next...)
	}
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
func (controller *scopeController) recordBlockedRetryWork(ctx context.Context, action *Action, scopeKey ScopeKey) {
	flow := controller.flow

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          action.Path,
		OldPath:       action.OldPath,
		ActionType:    action.Type,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		Blocked:       true,
	}, nil); err != nil {
		flow.engine.logger.Warn("failed to record blocked retry_work",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

func (controller *scopeController) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) bool {
	flow := controller.flow

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		HTTPStatus:    r.HTTPStatus,
		Blocked:       true,
	}, nil); err != nil {
		flow.engine.logger.Warn("failed to rehome blocked retry_work",
			slog.String("path", r.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	return true
}

// recordCascadeRetryWork records retry_work for a dependent whose parent failed.
func (controller *scopeController) recordCascadeRetryWork(
	ctx context.Context,
	watch *watchRuntime,
	action *Action,
	parentResult *ActionCompletion,
) {
	flow := controller.flow

	parentDecision := classifyResult(parentResult)
	scopeKey := parentDecision.ScopeEvidence
	blocked := flow.retryWorkShouldBeBlocked(watch, parentDecision.Class, scopeKey)

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          action.Path,
		OldPath:       action.OldPath,
		ActionType:    action.Type,
		ConditionType: parentDecision.ConditionType,
		ScopeKey:      scopeKey,
		LastError:     "parent action failed: " + parentResult.ErrMsg,
		HTTPStatus:    parentResult.HTTPStatus,
		Blocked:       blocked,
	}, retry.ReconcilePolicy().Delay); err != nil {
		flow.engine.logger.Warn("failed to record cascade retry_work",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}
