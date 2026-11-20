package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
	bl *syncdomain.Baseline,
) ([]*syncdomain.TrackedAction, error) {
	rt.mustAssertHeldReleaseAllowed("releaseDueHeldTrialsNow", "release due held trials")
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventTrialHeldReleaseStarted})

	dispatch, err := rt.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		return dispatch, err
	}
	rt.armHeldTimers()
	rt.deps.emit(syncdomain.EngineDebugEvent{
		Type:  syncdomain.EngineDebugEventTrialHeldReleaseCompleted,
		Count: len(dispatch),
	})
	return dispatch, nil
}

// releaseDueHeldRetriesNow releases due held retry entries that are already
// present in the runtime. It never rebuilds plan structure or revalidates
// durable rows.
func (rt *watchRuntime) releaseDueHeldRetriesNow(
	ctx context.Context,
	bl *syncdomain.Baseline,
) ([]*syncdomain.TrackedAction, error) {
	rt.mustAssertHeldReleaseAllowed("releaseDueHeldRetriesNow", "release due held retries")
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventRetryHeldReleaseStarted})

	dispatch, err := rt.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		return dispatch, err
	}
	rt.armHeldTimers()
	rt.deps.emit(syncdomain.EngineDebugEvent{
		Type:  syncdomain.EngineDebugEventRetryHeldReleaseCompleted,
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
func (flow *engineFlow) clearRetryWorkOnSuccess(ctx context.Context, r *syncdomain.ActionCompletion) error {
	if r == nil {
		return nil
	}

	return flow.clearRetryWorkOnActionSuccess(ctx, &syncdomain.Action{
		Path:    r.Path,
		OldPath: r.OldPath,
		Type:    r.ActionType,
	})
}

func (flow *engineFlow) clearRetryWorkOnActionSuccess(ctx context.Context, action *syncdomain.Action) error {
	// The action has already succeeded, so recording that is bookkeeping of
	// a finished effect: like the outcome commit it runs detached from
	// cancellation, or a shutdown that lands between the effect and this
	// write would leave the row behind and fail the completion. It has the
	// outcome commit's bound too (RunDetachedStoreAccess), so a wedged store
	// cannot hold a shutdown; in a stopping run's drain that is the drain's
	// one bound.
	if err := syncexec.RunDetachedStoreAccess(ctx, syncdomain.OutcomeCommitTimeout, func(clearCtx context.Context) error {
		return flow.resolveRetryWorkAndLogResolution(
			clearCtx,
			syncdomain.RetryWorkKeyForAction(action),
			retryResolutionSourceActionSuccess,
		)
	}); err != nil {
		return fmt.Errorf("clear retry work after success: %w", err)
	}

	return nil
}

func (flow *engineFlow) clearRetryWorkOnSuperseded(ctx context.Context, r *syncdomain.ActionCompletion, current *syncdomain.TrackedAction) error {
	if current != nil {
		return flow.resolveRetryWorkAndLogResolution(
			ctx,
			syncdomain.RetryWorkKeyForAction(&current.Action),
			retryResolutionSourceActionSuperseded,
		)
	}
	if r == nil {
		return nil
	}

	return flow.resolveRetryWorkAndLogResolution(
		ctx,
		syncdomain.RetryWorkKeyForCompletion(r),
		retryResolutionSourceActionSuperseded,
	)
}

func (flow *engineFlow) resolveRetryWorkAndLogResolution(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	resolutionSource string,
) error {
	row, found, err := flow.retryWork.resolve(ctx, flow.deps.store, work)
	if err != nil {
		return err
	}
	// The work succeeded: whatever the store had, nothing holds it any more.
	flow.retries.releaseHeldAction(work)
	if !found || row == nil {
		return nil
	}

	flow.deps.logger.Debug("retry_work resolved",
		slog.String("path", row.Path),
		slog.String("condition_type", string(syncstore.RetryConditionTypeForRow(row))),
		slog.String("action_type", row.ActionType.String()),
		slog.Int("attempt_count", row.AttemptCount),
		slog.String("resolution_source", resolutionSource),
	)

	return nil
}
