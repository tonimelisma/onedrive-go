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

func (flow *engineFlow) commitPublicationAction(ctx context.Context, ta *TrackedAction) error {
	mutation, err := publicationMutationFromAction(&ta.Action, flow.engine.driveID)
	if err == nil {
		err = flow.engine.baseline.CommitMutation(ctx, mutation)
	}
	return err
}

// reduceReadyFrontier keeps publication-only actions on the engine side of the
// boundary. On success it returns only concrete work for worker dispatch. If it
// returns an error, the returned slice contains any exact actions the caller
// still owns and should complete as shutdown instead of dispatching.
func (flow *engineFlow) reduceReadyFrontier(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	concrete := make([]*TrackedAction, 0, len(ready))
	queue := append([]*TrackedAction(nil), ready...)

	for len(queue) > 0 {
		ta := queue[0]
		queue = queue[1:]
		if ta == nil {
			continue
		}
		if !isPublicationOnlyActionType(ta.Action.Type) {
			concrete = append(concrete, ta)
			continue
		}

		if err := flow.commitPublicationAction(ctx, ta); err != nil {
			completion := actionCompletionFromTrackedAction(ta, nil, err)
			released, completionErr := flow.processActionCompletion(ctx, watch, &completion, bl)
			if completionErr != nil {
				return append(concrete, queue...), completionErr
			}
			queue = append(queue, released...)
			continue
		}
		unlocked, err := flow.applyPublicationSuccess(ctx, watch, ta)
		if err != nil {
			return append(concrete, queue...), err
		}
		queue = append(queue, unlocked...)
	}

	return concrete, nil
}
