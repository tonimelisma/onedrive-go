package sync

import (
	"context"
	"log/slog"
)

// processDirtyBatch refreshes current truth and replans from SQLite-owned
// comparison/reconciliation state. Dirty batches are scheduler hints only;
// the actionable set comes from snapshots plus baseline, never from the batch
// payload itself.
func (rt *watchRuntime) processDirtyBatch(
	ctx context.Context,
	batch DirtyBatch,
	bl *Baseline,
	mode Mode,
) []*TrackedAction {
	rt.beginSyncStatusBatch(rt.engine.nowFunc())
	rt.engine.logger.Info("processing watch dirty batch",
		slog.Int("paths", len(batch.Paths)),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(len(batch.Paths))

	observeStart := rt.engine.nowFunc()
	localResult, err := rt.observeLocalChanges(ctx, rt, bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch local refresh failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	commitErr := rt.commitObservedLocalSnapshot(ctx, false, localResult)
	if commitErr != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch local snapshot commit failed, skipping dirty batch",
			slog.String("error", commitErr.Error()),
		)
		return nil
	}
	rt.engine.collector().RecordObserve(len(batch.Paths), rt.engine.since(observeStart))

	planStart := rt.engine.nowFunc()
	plan, err := rt.buildCurrentActionPlan(ctx, bl, mode)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch sqlite planning failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	rt.engine.collector().RecordPlan(len(plan.Actions), rt.engine.since(planStart))

	reconcileErr := rt.reconcileDurablePlanState(ctx, plan)
	if reconcileErr != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch durable plan-state reconcile failed, skipping dirty batch",
			slog.String("error", reconcileErr.Error()),
		)
		return nil
	}

	retryRows, blockScopes, err := rt.loadPreparedRuntimeState(ctx)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch runtime state load failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}

	dispatch, dispatched, err := rt.startPreparedRuntime(ctx, &PreparedCurrentPlan{
		Plan:        plan,
		RetryRows:   retryRows,
		BlockScopes: blockScopes,
	}, bl, rt)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.engine.logger.Error("watch dispatch failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if !dispatched {
		rt.finishSyncStatusBatch(ctx, mode)
		return nil
	}
	rt.maybeFinishSyncStatusBatch(ctx, mode, dispatch)

	return dispatch
}
