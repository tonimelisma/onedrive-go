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
	batch DirtyBatch,
) error {
	if p == nil || p.bl == nil {
		return fmt.Errorf("sync: steady-state replan requires loaded baseline")
	}

	rt.beginSyncStatusBatch(rt.engine.nowFunc())
	rt.engine.logger.Info("processing watch steady-state replan",
		slog.Int("paths", len(batch.Paths)),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(len(batch.Paths))

	observeStart := rt.engine.nowFunc()
	localResult, err := rt.observeLocal(ctx, p.bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		if isWatchShutdownError(ctx, err) {
			return nil
		}
		rt.engine.logger.Error("watch local refresh failed, dropping replan trigger",
			slog.String("error", err.Error()),
		)
		return nil
	}
	findingsErr := rt.reconcileSkippedObservationFindings(ctx, localResult.Skipped)
	if findingsErr != nil {
		return rt.finishSteadyStateReplanStep(ctx, "local observation findings reconcile", findingsErr)
	}
	if rt.afterSteadyStateObserve != nil {
		rt.afterSteadyStateObserve()
	}
	commitErr := rt.commitObservedLocalSnapshot(ctx, false, localResult)
	if commitErr != nil {
		return rt.finishSteadyStateReplanStep(ctx, "local snapshot commit", commitErr)
	}
	rt.engine.collector().RecordObserve(len(batch.Paths), rt.engine.since(observeStart))

	prepared, err := rt.prepareSteadyStateCurrentPlan(ctx, p.bl, p.mode)
	if err != nil {
		return rt.finishSteadyStateReplanStep(ctx, "prepare", err)
	}

	dispatch, dispatched, err := rt.startPreparedRuntime(ctx, prepared, p.bl, rt)
	if err != nil {
		return rt.finishSteadyStateReplanStep(ctx, "start runtime", err)
	}
	rt.replaceOutbox(dispatch)
	if !dispatched {
		rt.finishSyncStatusBatch(ctx, p.mode)
		return nil
	}
	rt.maybeFinishSyncStatusBatch(ctx, p.mode, rt.currentOutbox())

	return nil
}

func (rt *watchRuntime) finishSteadyStateReplanStep(
	ctx context.Context,
	step string,
	err error,
) error {
	rt.clearSyncStatusBatch()
	if isWatchShutdownError(ctx, err) {
		rt.engine.logger.Debug("steady-state replan stopped by shutdown",
			slog.String("step", step),
			slog.String("error", err.Error()),
		)
		return nil
	}

	return fmt.Errorf("sync: watch replan %s: %w", step, err)
}
