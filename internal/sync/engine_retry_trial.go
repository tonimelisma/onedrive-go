package sync

import (
	"context"
	"fmt"
	"log/slog"
)

const (
	retryResolutionSourceActionSuccess    = "action_success"
	retryResolutionSourceActionSuperseded = "action_superseded"
)

// releaseDueHeldTrialsNow releases due held scope trials that are already
// present in the runtime. It never rebuilds plan structure or revalidates
// durable rows.
func (rt *watchRuntime) releaseDueHeldTrialsNow(
	ctx context.Context,
	bl *Baseline,
) ([]*trackedAction, error) {
	rt.mustAssertHeldReleaseAllowed("releaseDueHeldTrialsNow", "release due held trials")
	rt.deps.emit(engineDebugEvent{Type: engineDebugEventTrialHeldReleaseStarted})

	dispatch, err := rt.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		return dispatch, err
	}
	rt.armHeldTimers()
	rt.deps.emit(engineDebugEvent{
		Type:  engineDebugEventTrialHeldReleaseCompleted,
		Count: len(dispatch),
	})
	return dispatch, nil
}

// releaseDueHeldRetriesNow releases due held retry entries that are already
// present in the runtime. It never rebuilds plan structure or revalidates
// durable rows.
func (rt *watchRuntime) releaseDueHeldRetriesNow(
	ctx context.Context,
	bl *Baseline,
) ([]*trackedAction, error) {
	rt.mustAssertHeldReleaseAllowed("releaseDueHeldRetriesNow", "release due held retries")
	rt.deps.emit(engineDebugEvent{Type: engineDebugEventRetryHeldReleaseStarted})

	dispatch, err := rt.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		return dispatch, err
	}
	rt.armHeldTimers()
	rt.deps.emit(engineDebugEvent{
		Type:  engineDebugEventRetryHeldReleaseCompleted,
		Count: len(dispatch),
	})
	return dispatch, nil
}

// clearRetryWorkOnSuccess removes the retry_work row for a successfully
// completed action. The engine owns retry_work and observation-issue lifecycle;
// CommitMutation handles only baseline and remote_state updates. A store
// failure is returned like any other store write failure in the completion
// path: left behind, the row keeps `status` listing a file that just synced
// as retrying and schedules a redundant retry.
func (flow *engineFlow) clearRetryWorkOnSuccess(ctx context.Context, r *actionCompletion) error {
	if r == nil {
		return nil
	}

	return flow.clearRetryWorkOnActionSuccess(ctx, &Action{
		Path:    r.Path,
		OldPath: r.OldPath,
		Type:    r.ActionType,
	})
}

func (flow *engineFlow) clearRetryWorkOnActionSuccess(ctx context.Context, action *Action) error {
	if err := flow.resolveRetryWorkAndLogResolution(
		ctx,
		retryWorkKeyForAction(action),
		retryResolutionSourceActionSuccess,
	); err != nil {
		return fmt.Errorf("clear retry work after success: %w", err)
	}

	return nil
}

func (flow *engineFlow) clearRetryWorkOnSuperseded(ctx context.Context, r *actionCompletion, current *trackedAction) error {
	if current != nil {
		return flow.resolveRetryWorkAndLogResolution(
			ctx,
			retryWorkKeyForAction(&current.Action),
			retryResolutionSourceActionSuperseded,
		)
	}
	if r == nil {
		return nil
	}

	return flow.resolveRetryWorkAndLogResolution(
		ctx,
		retryWorkKeyForCompletion(r),
		retryResolutionSourceActionSuperseded,
	)
}

func (flow *engineFlow) resolveRetryWorkAndLogResolution(
	ctx context.Context,
	work RetryWorkKey,
	resolutionSource string,
) error {
	row, found, err := flow.deps.store.ResolveRetryWork(ctx, work)
	if err != nil {
		return fmt.Errorf("resolve retry work %s: %w", work.Path, err)
	}
	if !found || row == nil {
		return nil
	}

	delete(flow.retries.rowsByKey, work)
	flow.retries.releaseHeldAction(work)

	flow.deps.logger.Debug("retry_work resolved",
		slog.String("path", row.Path),
		slog.String("condition_type", retryConditionTypeForRow(row)),
		slog.String("action_type", row.ActionType.String()),
		slog.Int("attempt_count", row.AttemptCount),
		slog.String("resolution_source", resolutionSource),
	)

	return nil
}
