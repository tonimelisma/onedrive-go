package sync

import (
	"context"
	"fmt"
	"log/slog"
)

// runSteadyStateReplan is the single steady-state watch replan entry. Dirty
// batches are scheduler hints only; the actionable set always comes from
// committed current truth plus durable held-work state, never from the batch
// payload itself.
//
// Failure policy is explicit:
//   - local observation failure is recoverable and drops the batch
//   - once the engine depends on authoritative local snapshot or runtime state,
//     failures are fatal to watch
func (rt *watchRuntime) runSteadyStateReplan(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
) error {
	if p == nil || p.bl == nil {
		return fmt.Errorf("sync: steady-state replan requires loaded baseline")
	}

	rt.engine.logger.Info("processing watch steady-state replan",
		slog.Bool("dirty_signal", true),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(1)

	observeStart := rt.engine.nowFunc()
	localResult, err := rt.refreshAndCommitLocalCurrentState(ctx, p.bl, false)
	if err != nil {
		return rt.handleSteadyStateLocalRefreshError(ctx, err)
	}
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventSteadyStateObservationCompleted})
	rt.engine.collector().RecordObserve(len(localResult.Rows), rt.engine.since(observeStart))

	runtime, err := rt.runSteadyStateCurrentPlan(ctx, p.bl, p.mode)
	if err != nil {
		return rt.finishSteadyStateReplanStep(ctx, "build_current_plan", err)
	}

	dispatch, dispatched, err := rt.startRuntimeStage(ctx, runtime, p.bl, rt)
	if err != nil {
		return rt.finishSteadyStateReplanStep(ctx, "start_runtime", err)
	}
	rt.replaceOutbox(dispatch)
	if !dispatched {
		return nil
	}

	return nil
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
) error {
	if isWatchShutdownError(ctx, err) {
		return nil
	}

	step, ok := currentLocalRefreshStep(err)
	if !ok {
		return fmt.Errorf("sync: watch replan local refresh: %w", err)
	}
	if step == localCurrentRefreshStepObservation {
		rt.engine.logger.Error("watch local refresh failed, dropping replan trigger",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return rt.finishSteadyStateReplanStep(ctx, string(step), err)
}
