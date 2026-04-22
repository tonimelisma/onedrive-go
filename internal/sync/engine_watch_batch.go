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
	rt.engine.logger.Info("processing watch dirty batch",
		slog.Int("paths", len(batch.Paths)),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(len(batch.Paths))

	observeStart := rt.engine.nowFunc()
	localResult, err := rt.observeLocalChanges(ctx, rt, p.bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch local refresh failed, dropping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	commitErr := rt.commitObservedLocalSnapshot(ctx, false, localResult)
	if commitErr != nil {
		rt.clearSyncStatusBatch()
		return fmt.Errorf("sync: watch local snapshot commit: %w", commitErr)
	}
	rt.engine.collector().RecordObserve(len(batch.Paths), rt.engine.since(observeStart))

	prepared, err := rt.prepareDirtyCurrentPlan(ctx, p.bl, p.mode)
	if err != nil {
		rt.clearSyncStatusBatch()
		return fmt.Errorf("sync: watch replan prepare: %w", err)
	}

	dispatch, dispatched, err := rt.startPreparedRuntime(ctx, prepared, p.bl, rt)
	if err != nil {
		rt.clearSyncStatusBatch()
		return fmt.Errorf("sync: watch replan start runtime: %w", err)
	}
	rt.replaceOutbox(dispatch)
	if !dispatched {
		rt.finishSyncStatusBatch(ctx, p.mode)
		return nil
	}
	rt.maybeFinishSyncStatusBatch(ctx, p.mode, rt.currentOutbox())

	return nil
}
