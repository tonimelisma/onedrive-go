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
) ([]*trackedAction, bool, error) {
	if runtime == nil || runtime.Plan == nil {
		return nil, false, nil
	}

	plan := runtime.Plan
	if len(plan.Actions) != len(plan.Deps) {
		return nil, false, fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps))
	}

	flow.initializeRuntimeState(runtime)
	flow.sched.graph = newDepGraph(flow.deps.logger)

	if len(plan.Actions) == 0 {
		return nil, false, nil
	}

	ready := flow.registerPlanActions(plan)
	ready, err := flow.admitReady(ctx, ready)
	if err != nil {
		return nil, false, err
	}
	outbox, err := flow.reduceReadyFrontierStage(ctx, bl, ready)
	if err != nil {
		flow.completeOutboxAsShutdown(outbox)
		return nil, false, err
	}

	return outbox, flow.sched.graph.InFlightCount() > 0, nil
}

// reduceReadyFrontierStage owns the runtime handoff from "actions now ready"
// to "worker-dispatchable frontier". It keeps publication-only work on the
// engine side, releases already-due held retry/trial work, and re-runs
// publication drain on any newly released frontier.
func (flow *engineFlow) reduceReadyFrontierStage(
	ctx context.Context,
	bl *Baseline,
	ready []*trackedAction,
) ([]*trackedAction, error) {
	reduced, err := flow.runPublicationDrainStage(ctx, bl, ready)
	if err != nil {
		return reduced, err
	}

	dueHeld, err := flow.drainDueHeldWorkNow(ctx)
	if err != nil {
		return append(reduced, dueHeld...), err
	}
	if len(dueHeld) == 0 {
		return reduced, nil
	}

	released, err := flow.runPublicationDrainStage(ctx, bl, dueHeld)
	return append(reduced, released...), err
}

func (flow *engineFlow) registerPlanActions(plan *actionPlan) []*trackedAction {
	if flow.sched.graph == nil || plan == nil || len(plan.Actions) == 0 {
		return nil
	}

	actionIDs := flow.sched.allocatePlanActionIDs(len(plan.Actions))
	initialReady := make([]*trackedAction, 0, len(plan.Actions))

	for i := range plan.Actions {
		flow.sched.graph.Register(&plan.Actions[i], actionIDs[i])
	}

	for i := range plan.Actions {
		depIDs := make([]int64, 0, len(plan.Deps[i]))
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := flow.sched.graph.WireDeps(actionIDs[i], depIDs); ta != nil {
			initialReady = append(initialReady, ta)
		}
	}

	flow.deps.logger.Info("runtime plan registered",
		slog.Int("actions", len(plan.Actions)),
	)

	return initialReady
}
