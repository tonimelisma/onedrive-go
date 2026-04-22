package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (flow *engineFlow) failAfterControlStateError(current *TrackedAction, err error) error {
	flow.markFinished(current)
	return err
}

// applyRuntimeCompletionStage is the runtime completion boundary: classify the
// finished exact action, apply the resulting mutation/persistence decision, and
// then release any due held work back into the ready frontier.
func (flow *engineFlow) applyRuntimeCompletionStage(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	if r == nil {
		return nil, nil
	}

	decision := classifyResult(r)
	current := flow.trackedActionForCompletion(r)

	var (
		dispatched []*TrackedAction
		err        error
	)
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		dispatched, err = flow.applyTrialCompletionDecision(ctx, watch, r.TrialScopeKey, &decision, current, r, bl)
	} else {
		dispatched, err = flow.applyNormalCompletionDecision(ctx, watch, &decision, current, r, bl)
	}
	if err != nil {
		return dispatched, err
	}

	dueHeld, err := flow.drainDueHeldWorkNow(ctx, watch)
	if err != nil {
		return dispatched, err
	}
	return append(dispatched, dueHeld...), nil
}

func (flow *engineFlow) applyNormalCompletionDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	if handled, err := flow.maybeHandlePermissionOutcome(ctx, watch, decision, current, r, bl); handled {
		return nil, err
	}

	switch decision.Class {
	case errclass.ClassInvalid:
		if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		flow.markFinished(current)
		return nil, fmt.Errorf("classify action completion: invalid failure class")
	case errclass.ClassSuccess:
		dispatched, err := flow.applyCompletionSuccess(ctx, watch, current, r)
		if err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		return dispatched, nil
	case errclass.ClassShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "shutdown action completion")
		return nil, nil
	case errclass.ClassFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "fatal action completion")
		flow.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		if err := flow.applyOrdinaryFailureEffects(ctx, watch, current, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
	}

	return nil, nil
}

func (flow *engineFlow) applyTrialCompletionDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey ScopeKey,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	switch evaluateScopeTrialOutcome(trialScopeKey, decision) {
	case scopeTrialOutcomeRelease:
		dispatched, err := flow.applyTrialReleaseDecision(ctx, watch, current, r, trialScopeKey)
		if err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		return dispatched, nil
	case scopeTrialOutcomeShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "trial shutdown action completion")
		return nil, nil
	case scopeTrialOutcomeExtend:
		if err := flow.applyTrialExtendDecision(ctx, watch, current, r, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeRearmOrDiscard:
		if err := flow.applyTrialRearmOrDiscardDecision(ctx, watch, current, decision, r, bl, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "trial fatal action completion")
		flow.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	}

	return nil, nil
}

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

func (flow *engineFlow) applyPublicationMutation(ctx context.Context, ta *TrackedAction) error {
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

func (flow *engineFlow) failPublicationDrainAction(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	current *TrackedAction,
	cause error,
) ([]*TrackedAction, error) {
	completion := actionCompletionFromTrackedAction(current, nil, cause)
	return flow.applyRuntimeCompletionStage(ctx, watch, &completion, bl)
}

func (flow *engineFlow) applyPublicationDrainAction(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if err := flow.applyPublicationMutation(ctx, current); err != nil {
		return flow.failPublicationDrainAction(ctx, watch, bl, current, err)
	}

	return flow.completePublicationDrainAction(ctx, watch, current)
}

// runPublicationDrainStage keeps publication-only actions on the engine/store
// side of the runtime boundary. It durably applies publication work, routes
// publication failures through the same runtime completion stage, and returns
// only the remaining concrete worker frontier. If it returns an error, the
// returned slice contains exact actions the caller still owns and should
// complete as shutdown instead of dispatching.
func (flow *engineFlow) runPublicationDrainStage(
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

		released, err := flow.applyPublicationDrainAction(ctx, watch, bl, current)
		if err != nil {
			return append(concrete, queue...), err
		}

		nextConcrete, nextPublication := partitionPublicationFrontier(released)
		concrete = append(concrete, nextConcrete...)
		queue = append(queue, nextPublication...)
	}

	return concrete, nil
}
