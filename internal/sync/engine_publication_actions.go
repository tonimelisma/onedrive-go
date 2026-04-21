package sync

import (
	"context"
	"fmt"
)

func isPublicationOnlyActionType(actionType ActionType) bool {
	switch actionType {
	case ActionUpdateSynced, ActionCleanup:
		return true
	case ActionDownload,
		ActionUpload,
		ActionLocalDelete,
		ActionRemoteDelete,
		ActionLocalMove,
		ActionRemoteMove,
		ActionFolderCreate,
		ActionConflictCopy:
		return false
	}

	panic(fmt.Sprintf("unknown action type %d", actionType))
}

func (flow *engineFlow) commitPublicationAction(ctx context.Context, ta *TrackedAction) ActionCompletion {
	mutation, err := publicationMutationFromAction(&ta.Action, flow.engine.driveID)
	if err == nil {
		err = flow.engine.baseline.CommitMutation(ctx, mutation)
	}
	if err != nil {
		return completionFromTrackedAction(ta, nil, err)
	}

	return completionFromTrackedAction(ta, &ActionOutcome{
		Action:   ta.Action.Type,
		Success:  true,
		DriveID:  mutation.DriveID,
		ItemID:   mutation.ItemID,
		ItemType: mutation.ItemType,
	}, nil)
}

// reducePublicationFrontier keeps publication-only actions on the engine side
// of the boundary. It commits those actions synchronously, feeds their
// completions back through the normal engine result classifier, and returns
// only executable work for worker dispatch.
func (flow *engineFlow) reducePublicationFrontier(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	outbox []*TrackedAction,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	nextOutbox := append([]*TrackedAction(nil), outbox...)
	queue := append([]*TrackedAction(nil), ready...)

	for len(queue) > 0 {
		ta := queue[0]
		queue = queue[1:]
		if ta == nil {
			continue
		}
		if !isPublicationOnlyActionType(ta.Action.Type) {
			nextOutbox = append(nextOutbox, ta)
			continue
		}

		result := flow.commitPublicationAction(ctx, ta)
		outcome := flow.processActionCompletion(ctx, watch, &result, bl)
		if outcome.terminate {
			nextOutbox = append(nextOutbox, queue...)
			nextOutbox = append(nextOutbox, outcome.dispatched...)
			return nextOutbox, outcome.terminateErr
		}
		queue = append(queue, outcome.dispatched...)
	}

	return nextOutbox, nil
}
