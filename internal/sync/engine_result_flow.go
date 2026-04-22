package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
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

func (flow *engineFlow) failAfterControlStateError(current *TrackedAction, err error) error {
	flow.markFinished(current)
	return err
}

// processActionCompletion is the runtime completion boundary: classify the
// finished exact action, apply the resulting mutation/persistence decision, and
// then release any due held work back into the ready frontier.
func (flow *engineFlow) processActionCompletion(
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

	var dispatched []*TrackedAction
	var err error
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		dispatched, err = flow.processTrialDecision(ctx, watch, r.TrialScopeKey, &decision, current, r, bl)
	} else {
		dispatched, err = flow.processNormalDecision(ctx, watch, &decision, current, r, bl)
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

func (flow *engineFlow) applySuccessEffects(ctx context.Context, r *ActionCompletion) {
	flow.succeeded++
	flow.clearRetryWorkOnSuccess(ctx, r)
	if flow.scopeState != nil {
		flow.scopeState.RecordSuccess(r)
	}
}

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
) ([]*TrackedAction, error) {
	flow.markFinished(current)
	flow.applySuccessEffects(ctx, r)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, r.ActionID, "successful action completion")
}

func (flow *engineFlow) applyPublicationSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if current == nil {
		return nil, nil
	}

	flow.markFinished(current)
	flow.succeeded++
	flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, current.ID, "publication action completion")
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
	flow.scopeController().completeSubtree(ready)
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
		return flow.scopeController().feedScopeDetection(ctx, watch, r)
	}
	if decision.Class != errclass.ClassBlockScopeingTransient || decision.ScopeKey.IsZero() {
		return nil
	}

	return flow.scopeController().applyBlockScope(ctx, watch, ScopeUpdateResult{
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

func (flow *engineFlow) maybeHandlePermissionOutcome(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	scopeCtrl := flow.scopeController()

	if decision.PermissionFlow == permissionFlowNone {
		return false, nil
	}

	permOutcome, handled := scopeCtrl.decidePermissionOutcome(ctx, decision, r, bl)
	if !handled || !permOutcome.Matched {
		return false, nil
	}

	if _, err := scopeCtrl.applyPermissionOutcome(ctx, watch, decision.PermissionFlow, &permOutcome); err != nil {
		return true, flow.failAfterControlStateError(current, err)
	}
	if err := flow.applyPermissionOutcomeHold(ctx, watch, current, r, &permOutcome); err != nil {
		return true, flow.failAfterControlStateError(current, err)
	}
	if watch != nil {
		watch.armHeldTimers()
	}
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) applyPermissionOutcomeHold(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	permOutcome *PermissionOutcome,
) error {
	if permOutcome == nil {
		return nil
	}

	switch {
	case permOutcome.Kind == permissionOutcomeNone:
		if blocking := flow.findBlockingScope(current); !blocking.IsZero() {
			return flow.holdActionUnderScope(ctx, watch, current, r, blocking)
		}
		return nil
	case !permOutcome.ScopeKey.IsZero():
		return flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
	default:
		return flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
	}
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	scopeCtrl := flow.scopeController()

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
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		if err := flow.applyOrdinaryFailureEffects(ctx, watch, current, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
	}

	return nil, nil
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey ScopeKey,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	scopeCtrl := flow.scopeController()

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
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	}

	return nil, nil
}

func (flow *engineFlow) applyTrialReleaseDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	trialScopeKey ScopeKey,
) ([]*TrackedAction, error) {
	scopeCtrl := flow.scopeController()
	if err := scopeCtrl.releaseScope(ctx, watch, trialScopeKey); err != nil {
		return nil, err
	}
	flow.releaseHeldScope(trialScopeKey)

	return flow.applyCompletionSuccess(ctx, watch, current, r)
}

func (flow *engineFlow) applyTrialExtendDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	trialScopeKey ScopeKey,
) error {
	scopeCtrl := flow.scopeController()

	flow.markFinished(current)
	if err := scopeCtrl.rehomeBlockedRetryWork(ctx, r, trialScopeKey); err != nil {
		return err
	}
	if err := flow.holdActionUnderScope(ctx, watch, current, r, trialScopeKey); err != nil {
		return err
	}

	return scopeCtrl.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
}

func (flow *engineFlow) applyTrialRearmOrDiscardDecision(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
	trialScopeKey ScopeKey,
) error {
	scopeCtrl := flow.scopeController()

	flow.markFinished(current)
	reclassified, err := scopeCtrl.applyTrialReclassification(ctx, watch, decision, r, bl)
	if err != nil {
		return err
	}
	if reclassified {
		if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
			return err
		}
	} else {
		if err := flow.applyTrialRetryFallback(ctx, watch, current, decision, r); err != nil {
			return err
		}
	}

	return scopeCtrl.rearmOrDiscardScope(ctx, watch, trialScopeKey)
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
	return flow.scopeController().admitReady(ctx, watch, ready)
}

func (flow *engineFlow) holdActionFromPersistedRetryState(
	current *TrackedAction,
	work RetryWorkKey,
) error {
	if current == nil {
		return nil
	}

	row, ok := flow.retryRowsByKey[work]
	if !ok {
		return fmt.Errorf("hold action %s from persisted retry state: missing retry_work row", work.Path)
	}
	if row.Blocked {
		flow.holdAction(current, heldReasonScope, row.ScopeKey, time.Time{})
		return nil
	}

	nextRetry := time.Time{}
	if row.NextRetryAt > 0 {
		nextRetry = time.Unix(0, row.NextRetryAt)
	}
	flow.holdAction(current, heldReasonRetry, ScopeKey{}, nextRetry)
	return nil
}

func (flow *engineFlow) applyTrialRetryFallback(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
) error {
	if decision.Persistence != persistRetryWork {
		return fmt.Errorf("trial retry fallback for %s: missing retry_work persistence", r.Path)
	}
	if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
		return err
	}
	if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
		return err
	}
	if watch != nil {
		watch.armRetryTimer()
	}

	return nil
}

func (flow *engineFlow) holdActionUnderScope(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	if current == nil {
		return nil
	}

	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked by scope: " + scopeKey.String(),
		HTTPStatus:    r.HTTPStatus,
		Blocked:       true,
	}, nil)
	if err != nil {
		return fmt.Errorf("record blocked retry_work for %s under %s: %w", r.Path, scopeKey.String(), err)
	}
	if row == nil {
		return fmt.Errorf("record blocked retry_work for %s under %s: missing persisted row", r.Path, scopeKey.String())
	}
	flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
	flow.holdAction(current, heldReasonScope, scopeKey, time.Time{})
	if watch != nil {
		watch.armHeldTimers()
	}
	return nil
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
	watch *watchRuntime,
) ([]*TrackedAction, error) {
	now := flow.engine.nowFunc()
	var ready []*TrackedAction

	for _, key := range flow.dueRetryKeys(now) {
		if ta := flow.releaseHeldAction(key); ta != nil {
			ta.IsTrial = false
			ta.TrialScopeKey = ScopeKey{}
			ready = append(ready, ta)
		}
	}

	for _, key := range flow.dueTrialKeys(now) {
		held := flow.heldByKey[key]
		if held == nil {
			continue
		}
		ta := flow.releaseHeldAction(key)
		if ta == nil {
			continue
		}
		ta.IsTrial = true
		ta.TrialScopeKey = held.ScopeKey
		ready = append(ready, ta)
	}

	if len(ready) == 0 {
		return nil, nil
	}

	return flow.scopeController().admitReady(ctx, watch, ready)
}

func (controller *scopeController) clearBlockedRetryWorkForScope(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	if scopeKey.IsZero() {
		return nil
	}

	flow := controller.flow
	if err := flow.engine.baseline.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		return fmt.Errorf("clear blocked retry_work for %s under %s: %w", work.Path, scopeKey.String(), err)
	}
	if row, ok := flow.retryRowsByKey[work]; ok && row.Blocked && row.ScopeKey == scopeKey {
		delete(flow.retryRowsByKey, work)
	}

	return nil
}

func fatalResultError(r *ActionCompletion) error {
	if r.Err != nil {
		return fmt.Errorf("sync: unauthorized action completion for %s: %w", r.Path, r.Err)
	}

	return fmt.Errorf("sync: unauthorized action completion for %s", r.Path)
}

func (controller *scopeController) applyFatalAuthEffects(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	conditionKey ConditionKey,
) {
	flow := controller.flow
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

// decidePermissionOutcome is the shared engine-owned bridge between
// permission evidence gathering and pure policy. Normal completions and trial
// reclassification both call this helper so the engine makes one consistent
// evidence -> outcome decision before any persistence occurs.
func (controller *scopeController) decidePermissionOutcome(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) (PermissionOutcome, bool) {
	flow := controller.flow

	switch decision.PermissionFlow {
	case permissionFlowNone:
		return PermissionOutcome{}, false
	case permissionFlowRemote403:
		if bl == nil || !flow.engine.permHandler.HasPermChecker() {
			return PermissionOutcome{}, false
		}
		permEvidence := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType)
		return DecidePermissionOutcome(r, permEvidence), true
	case permissionFlowLocalPermission:
		permEvidence := flow.engine.permHandler.handleLocalPermission(ctx, r)
		return DecidePermissionOutcome(r, permEvidence), true
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
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
		LastError:     r.ErrMsg,
		HTTPStatus:    r.HTTPStatus,
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
