package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func (flow *engineFlow) startRuntimeStage(
	ctx context.Context,
	runtime *runtimePlan,
	bl syncdomain.BaselineView,
) ([]*syncdomain.TrackedAction, bool, error) {
	if runtime == nil || runtime.Plan == nil {
		return nil, false, nil
	}

	plan := runtime.Plan
	if len(plan.Actions) != len(plan.Deps) {
		return nil, false, fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps))
	}

	flow.initializeRuntimeState(runtime)
	flow.sched.NewGraph()

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

	return outbox, flow.sched.InFlight() > 0, nil
}

// reduceReadyFrontierStage owns the runtime handoff from "actions now ready"
// to "worker-dispatchable frontier". It keeps publication-only work on the
// engine side, releases already-due held retry/trial work, and re-runs
// publication drain on any newly released frontier. Every frontier leaves
// through the engine's dispatch order.
func (flow *engineFlow) reduceReadyFrontierStage(
	ctx context.Context,
	bl syncdomain.BaselineView,
	ready []*syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	frontier, err := flow.reduceReadyFrontier(ctx, bl, ready)
	if flow.hooks.dispatchOrder != nil && len(frontier) > 1 {
		frontier = flow.hooks.dispatchOrder(frontier)
	}

	return frontier, err
}

// reduceReadyFrontier applies the frontier's publication-only work and adds
// the held work that is due. A stopping run does neither (isStoppingRun): the
// ready set comes back as it is, for the caller to complete as shutdown, and
// held work stays held for the next sync.
func (flow *engineFlow) reduceReadyFrontier(
	ctx context.Context,
	bl syncdomain.BaselineView,
	ready []*syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	if isStoppingRun(ctx) {
		return ready, nil
	}

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

func (flow *engineFlow) registerPlanActions(plan *syncdomain.ActionPlan) []*syncdomain.TrackedAction {
	flow.pruneSupersedeStreaks(plan)
	if plan == nil || len(plan.Actions) == 0 {
		return nil
	}
	if flow.exec != nil {
		// A new plan reads every parent live again before its first
		// create into it; the memo is per plan.
		flow.exec.ResetRemoteParentChecks()
	}

	actionIDs := flow.sched.AllocateIDs(len(plan.Actions))
	initialReady := make([]*syncdomain.TrackedAction, 0, len(plan.Actions))

	for i := range plan.Actions {
		flow.sched.Register(&plan.Actions[i], actionIDs[i])
	}

	for i := range plan.Actions {
		depIDs := make([]int64, 0, len(plan.Deps[i]))
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := flow.sched.WireDeps(actionIDs[i], depIDs); ta != nil {
			initialReady = append(initialReady, ta)
		}
	}

	flow.deps.logger.Info("runtime plan registered",
		slog.Int("actions", len(plan.Actions)),
	)

	return initialReady
}
