package sync

import (
	"context"
	"fmt"
	"log/slog"
)

func (flow *engineFlow) startRuntimeStage(
	ctx context.Context,
	runtime *runtimePlan,
	bl *Baseline,
	watch *watchRuntime,
) ([]*TrackedAction, bool, error) {
	if runtime == nil || runtime.Plan == nil {
		return nil, false, nil
	}

	plan := runtime.Plan
	if len(plan.Actions) != len(plan.Deps) {
		return nil, false, fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps))
	}

	flow.initializeRuntimeState(runtime)
	flow.depGraph = NewDepGraph(flow.engine.logger)

	if len(plan.Actions) == 0 {
		return nil, false, nil
	}

	ready := flow.registerPlanActions(plan)
	ready, err := flow.admitReady(ctx, watch, ready)
	if err != nil {
		return nil, false, err
	}
	outbox, err := flow.reduceReadyFrontierStage(ctx, watch, bl, ready)
	if err != nil {
		flow.completeOutboxAsShutdown(outbox)
		return nil, false, err
	}

	return outbox, flow.depGraph.InFlightCount() > 0, nil
}

// reduceReadyFrontierStage owns the runtime handoff from "actions now ready"
// to "worker-dispatchable frontier". It keeps publication-only work on the
// engine side, releases already-due held retry/trial work, and re-runs
// publication drain on any newly released frontier.
func (flow *engineFlow) reduceReadyFrontierStage(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	reduced, err := flow.runPublicationDrainStage(ctx, watch, bl, ready)
	if err != nil {
		return reduced, err
	}

	dueHeld, err := flow.drainDueHeldWorkNow(ctx, watch)
	if err != nil {
		return append(reduced, dueHeld...), err
	}
	if len(dueHeld) == 0 {
		return reduced, nil
	}

	released, err := flow.runPublicationDrainStage(ctx, watch, bl, dueHeld)
	return append(reduced, released...), err
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
