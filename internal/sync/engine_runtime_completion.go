package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
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

	return flow.reduceReadyFrontierStage(ctx, watch, bl, dispatched)
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

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
) ([]*TrackedAction, error) {
	return flow.applyTrackedActionSuccess(ctx, watch, current, r, "successful action completion")
}

func (flow *engineFlow) applyTrackedActionSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	depGraphReason string,
) ([]*TrackedAction, error) {
	flow.markFinished(current)
	flow.succeeded++
	if current != nil {
		flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	} else {
		flow.clearRetryWorkOnSuccess(ctx, r)
	}
	if flow.scopeState != nil && r != nil {
		flow.scopeState.RecordSuccess(r)
	}

	actionID := int64(0)
	switch {
	case current != nil:
		actionID = current.ID
	case r != nil:
		actionID = r.ActionID
	default:
		return nil, nil
	}

	return flow.admitReadyAfterSuccessfulAction(ctx, watch, actionID, depGraphReason)
}

// applyOrdinaryFailureEffects handles post-routing side effects for normal
// worker failures. Trial results intentionally use separate scope-relative
// policy so they do not accidentally mutate the original scope via generic
// failure recording or scope detection.
func (flow *engineFlow) applyOrdinaryFailureEffects(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
) error {
	persisted, err := flow.persistAndHoldFailure(ctx, current, decision, r)
	if err != nil {
		return err
	}
	if err := flow.applyPersistedFailureScopeEffects(ctx, watch, decision, r, persisted); err != nil {
		return err
	}
	flow.armFailureTimers(watch, decision, persisted)

	flow.recordError(decision, r)
	return nil
}

func (flow *engineFlow) persistAndHoldFailure(
	ctx context.Context,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
) (bool, error) {
	if decision.Persistence != persistRetryWork {
		return false, nil
	}
	if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
		return false, err
	}
	if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
		return false, err
	}

	return true, nil
}

func (flow *engineFlow) applyPersistedFailureScopeEffects(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	persisted bool,
) error {
	if !persisted {
		return nil
	}
	if decision.RunScopeDetection {
		return flow.feedScopeDetection(ctx, watch, r)
	}
	if decision.Class != errclass.ClassBlockScopeingTransient || decision.ScopeKey.IsZero() {
		return nil
	}

	return flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
		Block:         true,
		ScopeKey:      decision.ScopeKey,
		ConditionType: decision.ScopeKey.ConditionType(),
	})
}

func (flow *engineFlow) armFailureTimers(
	watch *watchRuntime,
	decision *ResultDecision,
	persisted bool,
) {
	if watch == nil {
		return
	}
	if decision.Class == errclass.ClassBlockScopeingTransient {
		watch.armTrialTimer()
	}
	if persisted {
		watch.armRetryTimer()
	}
}

func fatalResultError(r *ActionCompletion) error {
	if r.Err != nil {
		return fmt.Errorf("sync: unauthorized action completion for %s: %w", r.Path, r.Err)
	}

	return fmt.Errorf("sync: unauthorized action completion for %s", r.Path)
}

func (flow *engineFlow) applyFatalAuthEffects(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	conditionKey ConditionKey,
) {
	logFields := flow.summaryLogFields(
		errclass.ClassFatal,
		conditionKey,
		r.Path,
		ScopeKey{},
	)

	if flow.engine.permHandler != nil && flow.engine.permHandler.accountEmail != "" {
		if err := config.MarkAccountAuthRequired(
			flow.engine.dataDir,
			flow.engine.permHandler.accountEmail,
			authstate.ReasonSyncAuthRejected,
		); err != nil {
			fields := append([]any{}, logFields...)
			fields = append(fields,
				slog.String("account", flow.engine.permHandler.accountEmail),
				slog.String("error", err.Error()),
			)
			flow.engine.logger.Warn("fatal unauthorized: failed to persist catalog auth requirement", fields...)
		}
	}

	flow.engine.logger.Error("authentication required: sync stopping",
		logFields...,
	)

	_ = ctx
	_ = watch
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

func (flow *engineFlow) completePublicationDrainAction(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if current == nil {
		return nil, nil
	}

	return flow.applyTrackedActionSuccess(ctx, watch, current, nil, "successful action completion")
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

func (flow *engineFlow) recordRetryWork(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	delayFn func(int) time.Duration,
) error {
	scopeKey := decision.ScopeEvidence
	blocked := flow.retryWorkShouldBeBlocked(decision.Class, scopeKey)

	if decision.Class == errclass.ClassActionable {
		fields := append(flow.resultLogFields(decision, r),
			slog.String("condition_type", decision.ConditionType),
			slog.String("scope_evidence", decision.ScopeEvidence.String()),
		)
		flow.engine.logger.Debug(
			"execution recorded retry_work for a current-truth condition; observation may suppress the next plan and prune it",
			fields...,
		)
	}

	row, recErr := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: decision.ConditionType,
		ScopeKey:      scopeKey,
		Blocked:       blocked,
	}, delayFn)
	if recErr != nil {
		return fmt.Errorf("record retry_work for %s: %w", r.Path, recErr)
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("condition_type", decision.ConditionType),
		slog.String("error", r.ErrMsg),
		slog.String("scope_evidence", scopeKey.String()),
		slog.Bool("blocked", blocked),
	)
	if row == nil {
		return fmt.Errorf("record retry_work for %s: missing persisted row", r.Path)
	}
	flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
	fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	flow.engine.logger.Debug("retry_work recorded", fields...)

	return nil
}

func (flow *engineFlow) retryWorkShouldBeBlocked(
	class errclass.Class,
	scopeKey ScopeKey,
) bool {
	if scopeKey.IsZero() {
		return false
	}
	if class == errclass.ClassBlockScopeingTransient {
		return true
	}
	if class != errclass.ClassRetryableTransient {
		return false
	}

	return flow.hasActiveScope(scopeKey)
}

func (flow *engineFlow) applyResultPersistence(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
) error {
	switch decision.Persistence {
	case persistNone:
		return nil
	case persistRetryWork:
		return flow.recordRetryWork(ctx, decision, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}
