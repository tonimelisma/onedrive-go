package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const retryResolutionSourceWorkerSuccess = "worker_success"

// runTrialDispatch releases due held scope trials that are already present in
// the runtime. It never rebuilds plan structure or revalidates durable rows.
func (rt *watchRuntime) runTrialDispatch(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) []*TrackedAction {
	_ = bl
	_ = mode

	rt.mustAssertPlannerSweepAllowed(rt, "runTrialDispatch", "run trial dispatch")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventTrialSweepStarted})

	dispatch := rt.drainDueHeldWorkNow(ctx, rt)
	rt.armHeldTimers()
	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventTrialSweepCompleted,
		Count: len(dispatch),
	})
	return dispatch
}

// runRetrierSweep releases due held retry entries that are already present in
// the runtime. It never rebuilds plan structure or revalidates durable rows.
func (rt *watchRuntime) runRetrierSweep(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) []*TrackedAction {
	_ = bl
	_ = mode

	rt.mustAssertPlannerSweepAllowed(rt, "runRetrierSweep", "run retrier sweep")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepStarted})

	dispatch := rt.drainDueHeldWorkNow(ctx, rt)
	rt.armHeldTimers()
	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventRetrySweepCompleted,
		Count: len(dispatch),
	})
	return dispatch
}

// clearRetryWorkOnSuccess removes the retry_work row for a successfully
// completed action. The engine owns retry_work and observation-issue lifecycle;
// CommitMutation handles only baseline and remote_state updates.
func (flow *engineFlow) clearRetryWorkOnSuccess(ctx context.Context, r *ActionCompletion) {
	if r == nil {
		return
	}
	flow.clearRetryWorkOnActionSuccess(ctx, &Action{
		Path:    r.Path,
		OldPath: r.OldPath,
		Type:    r.ActionType,
	})
}

func (flow *engineFlow) clearRetryWorkOnActionSuccess(ctx context.Context, action *Action) {
	if clearErr := flow.resolveRetryWorkAndLogResolution(
		ctx,
		retryWorkKeyForAction(action),
		retryResolutionSourceWorkerSuccess,
	); clearErr != nil {
		path := ""
		if action != nil {
			path = action.Path
		}
		flow.engine.logger.Warn("failed to clear retry_work on success",
			slog.String("path", path),
			slog.String("error", clearErr.Error()),
		)
	}
}

func (flow *engineFlow) resolveRetryWorkAndLogResolution(
	ctx context.Context,
	work RetryWorkKey,
	resolutionSource string,
) error {
	row, found, err := flow.engine.baseline.ResolveRetryWork(ctx, work)
	if err != nil {
		return fmt.Errorf("resolve retry work %s: %w", work.Path, err)
	}
	if !found || row == nil {
		return nil
	}

	delete(flow.retryRowsByKey, work)
	flow.releaseHeldAction(work)

	flow.engine.logger.Info("retry_work resolved",
		slog.String("path", row.Path),
		slog.String("condition_type", row.ConditionType),
		slog.String("action_type", row.ActionType.String()),
		slog.Int("attempt_count", row.AttemptCount),
		slog.String("resolution_source", resolutionSource),
	)

	return nil
}

func (flow *engineFlow) applyResultPersistence(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
) bool {
	switch decision.Persistence {
	case persistNone:
		return true
	case persistRetryWork:
		return flow.recordRetryWork(ctx, decision, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}
