package sync

import (
	"context"
	"fmt"
	"log/slog"
)

func (flow *engineFlow) startPreparedRuntime(
	ctx context.Context,
	prepared *PreparedCurrentPlan,
	bl *Baseline,
	watch *watchRuntime,
) ([]*TrackedAction, bool, error) {
	if prepared == nil || prepared.Plan == nil {
		return nil, false, nil
	}

	plan := prepared.Plan
	if len(plan.Actions) != len(plan.Deps) {
		return nil, false, fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps))
	}

	flow.initializePreparedRuntime(prepared)
	flow.depGraph = NewDepGraph(flow.engine.logger)

	if len(plan.Actions) == 0 {
		return nil, false, nil
	}

	ready := flow.registerPlanActions(plan)
	ready = flow.scopeController().admitReady(ctx, watch, ready)
	outbox, err := flow.reducePublicationFrontier(ctx, watch, bl, nil, ready)
	if err != nil {
		flow.completeOutboxAsShutdown(outbox)
		return nil, false, err
	}
	dueHeld := flow.drainDueHeldWorkNow(ctx, watch)
	if len(dueHeld) > 0 {
		outbox, err = flow.reducePublicationFrontier(ctx, watch, bl, outbox, dueHeld)
		if err != nil {
			flow.completeOutboxAsShutdown(outbox)
			return nil, false, err
		}
	}

	return outbox, flow.depGraph.InFlightCount() > 0, nil
}

func (flow *engineFlow) registerPlanActions(plan *ActionPlan) []*TrackedAction {
	if flow.depGraph == nil || plan == nil || len(plan.Actions) == 0 {
		return nil
	}

	actionIDs := flow.allocatePlanActionIDs(len(plan.Actions))
	initialReady := make([]*TrackedAction, 0, len(plan.Actions))

	for i := range plan.Actions {
		flow.depGraph.Register(&plan.Actions[i], actionIDs[i])
	}

	for i := range plan.Actions {
		depIDs := make([]int64, 0, len(plan.Deps[i]))
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := flow.depGraph.WireDeps(actionIDs[i], depIDs); ta != nil {
			initialReady = append(initialReady, ta)
		}
	}

	flow.engine.logger.Info("runtime plan registered",
		slog.Int("actions", len(plan.Actions)),
	)

	return initialReady
}

func (flow *engineFlow) allocatePlanActionIDs(count int) []int64 {
	actionIDs := make([]int64, count)
	baseID := flow.nextActionID
	flow.nextActionID += int64(count)

	for i := 0; i < count; i++ {
		actionIDs[i] = baseID + int64(i)
	}

	return actionIDs
}
