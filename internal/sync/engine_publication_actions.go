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

// reducePublicationFrontier keeps publication-only actions on the engine side
// of the boundary. It commits those actions synchronously, unlocks their
// dependents directly through the engine-owned publication success path, and
// returns only executable work for worker dispatch.
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

		if err := flow.commitPublicationAction(ctx, ta); err != nil {
			nextOutbox = append(nextOutbox, queue...)
			return nextOutbox, err
		}
		queue = append(queue, flow.applyPublicationSuccess(ctx, watch, ta)...)
	}

	_ = bl
	return nextOutbox, nil
}
