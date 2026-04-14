package sync

import (
	"context"
	"fmt"
)

func (rt *watchRuntime) handleWatchMutationRequest(
	ctx context.Context,
	request *WatchMutationRequest,
) watchTransition {
	if request == nil {
		return watchTransition{}
	}

	switch request.Kind {
	case WatchMutationApproveHeldDeletes:
		err := rt.engine.baseline.ApproveHeldDeletes(ctx)
		if err == nil && rt.deleteCounter != nil {
			rt.deleteCounter.Release()
		}
		request.respond(&WatchMutationResponse{Err: err})
		if err != nil {
			return watchTransition{}
		}

		return watchTransition{markUserIntentPending: true}
	case WatchMutationRequestConflictResolution:
		result, err := rt.engine.baseline.RequestConflictResolution(ctx, request.ConflictID, request.Resolution)
		request.respond(&WatchMutationResponse{
			ConflictRequestResult: result,
			Err:                   err,
		})
		if err != nil {
			return watchTransition{}
		}

		switch result.Status {
		case ConflictRequestQueued, ConflictRequestAlreadyQueued:
			return watchTransition{markUserIntentPending: true}
		case ConflictRequestAlreadyApplying, ConflictRequestAlreadyResolved:
			return watchTransition{}
		default:
			return watchTransition{}
		}
	default:
		request.respond(&WatchMutationResponse{
			Err: fmt.Errorf("sync: unknown watch mutation kind %q", request.Kind),
		})
		return watchTransition{}
	}
}
