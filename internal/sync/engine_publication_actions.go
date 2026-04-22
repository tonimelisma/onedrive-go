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

func (flow *engineFlow) commitPublicationMutation(ctx context.Context, ta *TrackedAction) error {
	mutation, err := publicationMutationFromAction(&ta.Action, flow.engine.driveID)
	if err == nil {
		err = flow.engine.baseline.CommitMutation(ctx, mutation)
	}
	return err
}

func partitionPublicationFrontier(ready []*TrackedAction) ([]*TrackedAction, []*TrackedAction) {
	concrete := make([]*TrackedAction, 0, len(ready))
	publication := make([]*TrackedAction, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}
		if isPublicationOnlyActionType(ta.Action.Type) {
			publication = append(publication, ta)
			continue
		}
		concrete = append(concrete, ta)
	}

	return concrete, publication
}

func (flow *engineFlow) drainPublicationFailure(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	current *TrackedAction,
	cause error,
) ([]*TrackedAction, error) {
	completion := actionCompletionFromTrackedAction(current, nil, cause)
	return flow.processActionCompletion(ctx, watch, &completion, bl)
}

func (flow *engineFlow) drainPublicationAction(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if err := flow.commitPublicationMutation(ctx, current); err != nil {
		return flow.drainPublicationFailure(ctx, watch, bl, current, err)
	}

	return flow.drainPublicationSuccess(ctx, watch, current)
}

// drainPublicationFrontier keeps publication-only actions on the engine/store
// side of the runtime boundary. It durably applies publication work, routes
// publication failures through normal completion handling, and returns only the
// remaining concrete worker frontier. If it returns an error, the returned slice
// contains exact actions the caller still owns and should complete as shutdown
// instead of dispatching.
func (flow *engineFlow) drainPublicationFrontier(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	ready []*TrackedAction,
) ([]*TrackedAction, error) {
	concrete, publication := partitionPublicationFrontier(ready)
	queue := append([]*TrackedAction(nil), publication...)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		released, err := flow.drainPublicationAction(ctx, watch, bl, current)
		if err != nil {
			return append(concrete, queue...), err
		}

		nextConcrete, nextPublication := partitionPublicationFrontier(released)
		concrete = append(concrete, nextConcrete...)
		queue = append(queue, nextPublication...)
	}

	return concrete, nil
}
