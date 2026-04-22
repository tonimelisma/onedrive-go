package sync

import (
	"context"
	"fmt"
)

func (flow *engineFlow) completeDepGraphAction(actionID int64, reason string) []*TrackedAction {
	if flow.depGraph == nil {
		panic(fmt.Sprintf("dep_graph: complete action %d during %s with nil graph", actionID, reason))
	}

	ready, ok := flow.depGraph.Complete(actionID)
	if !ok {
		panic(fmt.Sprintf("dep_graph: complete unknown action ID %d during %s", actionID, reason))
	}

	return ready
}

func (flow *engineFlow) applyCompletedSubtree(
	current *TrackedAction,
	actionID int64,
	reason string,
) {
	flow.markFinished(current)
	if current == nil {
		return
	}

	ready := flow.completeDepGraphAction(actionID, reason)
	flow.completeSubtree(ready)
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown.
func (flow *engineFlow) completeSubtree(ready []*TrackedAction) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next := flow.completeDepGraphAction(current.ID, "completeSubtree")
		queue = append(queue, next...)
	}
}

func (flow *engineFlow) trackedActionForCompletion(r *ActionCompletion) *TrackedAction {
	if r == nil || flow.depGraph == nil {
		return nil
	}

	ta, ok := flow.depGraph.Get(r.ActionID)
	if !ok {
		return nil
	}

	return ta
}

func (flow *engineFlow) admitReadyAfterSuccessfulAction(
	ctx context.Context,
	watch *watchRuntime,
	actionID int64,
	reason string,
) ([]*TrackedAction, error) {
	ready := flow.completeDepGraphAction(actionID, reason)
	return flow.admitReady(ctx, watch, ready)
}
