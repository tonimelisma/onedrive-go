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

func publicationFailureCompletion(ta *TrackedAction, err error) *ActionCompletion {
	if ta == nil {
		return nil
	}

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}

	return &ActionCompletion{
		ActionID:      ta.ID,
		Path:          ta.Action.Path,
		OldPath:       ta.Action.OldPath,
		DriveID:       ta.Action.DriveID,
		TargetDriveID: ta.Action.TargetDriveID,
		ActionType:    ta.Action.Type,
		Err:           err,
		ErrMsg:        errMsg,
		IsTrial:       ta.IsTrial,
		TrialScopeKey: ta.TrialScopeKey,
	}
}

// reducePublicationFrontier keeps publication-only actions on the engine side
// of the boundary. It commits those actions synchronously, unlocks their
// dependents directly through the engine-owned publication success path, and
// returns only executable work for worker dispatch. Publication failures still
// route through the ordinary result classifier so exact retry_work persists and
// the current runtime can hold the failed publication node instead of tearing
// down the whole loop on a transient store error.
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
			outcome := flow.processActionCompletion(ctx, watch, publicationFailureCompletion(ta, err), bl)
			if outcome.terminate {
				nextOutbox = append(nextOutbox, queue...)
				return nextOutbox, outcome.terminateErr
			}
			queue = append(queue, outcome.dispatched...)
			continue
		}
		queue = append(queue, flow.applyPublicationSuccess(ctx, watch, ta)...)
	}

	_ = bl
	return nextOutbox, nil
}
