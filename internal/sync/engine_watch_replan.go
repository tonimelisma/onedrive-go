package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// runSteadyStateReplan is the single steady-state watch replan entry. Dirty
// batches are scheduler hints only; the actionable set always comes from
// committed current truth plus durable held-work state, never from the batch
// payload itself.
//
// Failure policy is explicit:
//   - local observation failure is recoverable and reports applied=false
//   - once the engine depends on authoritative local snapshot or runtime state,
//     failures are fatal to watch
func (rt *watchRuntime) runSteadyStateReplan(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
) (bool, error) {
	if p == nil || p.bl == nil {
		return false, fmt.Errorf("sync: steady-state replan requires loaded baseline")
	}

	rt.engine.logger.Info("processing watch steady-state replan",
		slog.Bool("dirty_signal", true),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(1)

	replanStart := rt.engine.nowFunc()
	rt.emitRuntimeDebugEvent(engineDebugEventSteadyStateReplanStarted, "", 0, time.Time{})

	observeStart := rt.engine.nowFunc()
	rt.emitRuntimeDebugEvent(engineDebugEventLocalTruthRefreshStarted, "", 0, replanStart)
	localResult, err := rt.refreshAndCommitLocalCurrentState(ctx, p.bl)
	if err != nil {
		return rt.handleSteadyStateLocalRefreshError(ctx, err)
	}
	rt.emitRuntimeDebugEvent(engineDebugEventLocalTruthRefreshFinished, "", len(localResult.Rows), observeStart)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventSteadyStateObservationCompleted})
	rt.engine.collector().RecordObserve(len(localResult.Rows), rt.engine.since(observeStart))

	planStart := rt.engine.nowFunc()
	rt.emitRuntimeDebugEvent(engineDebugEventPlanningStarted, "", 0, replanStart)
	runtime, err := rt.runSteadyStateCurrentPlan(ctx, p.bl, p.mode)
	if err != nil {
		return false, rt.finishSteadyStateReplanStep(ctx, "build_current_plan", err)
	}
	rt.emitRuntimeDebugEvent(engineDebugEventPlanningFinished, "", 0, planStart)

	dispatch, _, err := rt.startRuntimeStage(ctx, runtime, p.bl, rt)
	if err != nil {
		return false, rt.finishSteadyStateReplanStep(ctx, "start_runtime", err)
	}
	rt.replaceOutbox(dispatch)
	rt.loop.postReplanOutbox = len(dispatch) > 0
	rt.emitRuntimeDebugEvent(engineDebugEventNewPlanInstalled, "", len(dispatch), replanStart)

	return true, nil
}

func (rt *watchRuntime) finishSteadyStateReplanStep(
	ctx context.Context,
	step string,
	err error,
) error {
	if isWatchShutdownError(ctx, err) {
		rt.engine.logger.Debug("steady-state replan stopped by shutdown",
			slog.String("step", step),
			slog.String("error", err.Error()),
		)
		return nil
	}

	return fmt.Errorf("sync: watch replan %s: %w", step, err)
}

func (rt *watchRuntime) handleSteadyStateLocalRefreshError(
	ctx context.Context,
	err error,
) (bool, error) {
	if isWatchShutdownError(ctx, err) {
		return false, nil
	}

	step, ok := currentLocalRefreshStep(err)
	if !ok {
		return false, fmt.Errorf("sync: watch replan local refresh: %w", err)
	}
	if step == localCurrentRefreshStepObservation {
		rt.engine.logger.Error("watch local refresh failed before runtime replacement",
			slog.String("error", err.Error()),
		)
		return false, nil
	}

	return false, rt.finishSteadyStateReplanStep(ctx, string(step), err)
}
