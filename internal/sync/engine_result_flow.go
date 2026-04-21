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

type resultContext struct {
	isTrial       bool
	trialScopeKey ScopeKey
}

type routeOutcome struct {
	dispatched   []*TrackedAction
	terminate    bool
	terminateErr error
}

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

// processActionCompletion owns completion classification plus failure-aware
// dependent dispatch.
func (flow *engineFlow) processActionCompletion(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	if r == nil {
		return routeOutcome{}
	}

	var outcome routeOutcome
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		outcome = flow.processResult(ctx, watch, resultContext{
			isTrial:       true,
			trialScopeKey: r.TrialScopeKey,
		}, r, bl)
	} else {
		outcome = flow.processResult(ctx, watch, resultContext{}, r, bl)
	}

	if outcome.terminate {
		return outcome
	}

	outcome.dispatched = append(outcome.dispatched, flow.drainDueHeldWorkNow(ctx, watch)...)
	return outcome
}

func (flow *engineFlow) processResult(
	ctx context.Context,
	watch *watchRuntime,
	resultCtx resultContext,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	decision := classifyResult(r)
	ta := flow.trackedActionForCompletion(r)

	if resultCtx.isTrial {
		return flow.processTrialDecision(ctx, watch, resultCtx.trialScopeKey, &decision, ta, r, bl)
	}

	return flow.processNormalDecision(ctx, watch, &decision, ta, r, bl)
}

func (flow *engineFlow) applySuccessEffects(ctx context.Context, r *ActionCompletion) {
	flow.succeeded++
	flow.clearRetryWorkOnSuccess(ctx, r)
	if flow.scopeState != nil {
		flow.scopeState.RecordSuccess(r)
	}
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
) {
	persisted := flow.applyResultPersistence(ctx, decision, r)
	if persisted {
		flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
	}

	if persisted && decision.RunScopeDetection {
		flow.scopeController().feedScopeDetection(ctx, watch, r)
	} else if persisted && decision.Class == errclass.ClassBlockScopeingTransient && !decision.ScopeKey.IsZero() {
		flow.scopeController().applyBlockScope(ctx, watch, ScopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ScopeKey.ConditionType(),
		})
	}

	if decision.Class == errclass.ClassBlockScopeingTransient && watch != nil {
		watch.armTrialTimer()
	}
	if persisted && decision.Persistence == persistRetryWork && watch != nil {
		watch.armRetryTimer()
	}

	flow.recordError(decision, r)
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	scopeCtrl := flow.scopeController()

	if decision.PermissionFlow != permissionFlowNone {
		if permOutcome, handled := scopeCtrl.decidePermissionOutcome(ctx, decision, r, bl); handled {
			if permOutcome.Matched {
				scopeCtrl.applyPermissionOutcome(ctx, watch, decision.PermissionFlow, &permOutcome)
				switch {
				case permOutcome.Kind == permissionOutcomeNone:
					if blocking := flow.findBlockingScope(current); !blocking.IsZero() {
						flow.holdActionUnderScope(ctx, watch, current, r, blocking)
					}
				case !permOutcome.ScopeKey.IsZero():
					flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
				default:
					flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
				}
				if watch != nil {
					watch.armHeldTimers()
				}
				flow.recordError(decision, r)
				return routeOutcome{}
			}
		}
	}

	outcome := routeOutcome{}

	switch decision.Class {
	case errclass.ClassInvalid:
		flow.applyResultPersistence(ctx, decision, r)
		flow.markFinished(current)
		outcome.terminate = true
		outcome.terminateErr = fmt.Errorf("classify action completion: invalid failure class")
	case errclass.ClassSuccess:
		flow.markFinished(current)
		flow.applySuccessEffects(ctx, r)
		outcome.dispatched = flow.readyAfterSuccess(ctx, watch, r.ActionID)
	case errclass.ClassShutdown:
		flow.markFinished(current)
		if current != nil {
			ready := flow.completeDepGraphAction(r.ActionID, "shutdown action completion")
			scopeCtrl.completeSubtree(ready)
		}
		return outcome
	case errclass.ClassFatal:
		flow.markFinished(current)
		if current != nil {
			ready := flow.completeDepGraphAction(r.ActionID, "fatal action completion")
			scopeCtrl.completeSubtree(ready)
		}
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		flow.applyOrdinaryFailureEffects(ctx, watch, current, decision, r)
	}

	return outcome
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey ScopeKey,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	scopeCtrl := flow.scopeController()
	outcome := routeOutcome{}

	switch evaluateScopeTrialOutcome(trialScopeKey, decision) {
	case scopeTrialOutcomeRelease:
		flow.markFinished(current)
		flow.applySuccessEffects(ctx, r)
		if err := scopeCtrl.releaseScope(ctx, watch, trialScopeKey); err != nil {
			flow.engine.logger.Warn("trial result: failed to release scope",
				slog.String("scope_key", trialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		flow.releaseHeldScope(trialScopeKey)
		outcome.dispatched = flow.readyAfterSuccess(ctx, watch, r.ActionID)
	case scopeTrialOutcomeShutdown:
		flow.markFinished(current)
		if current != nil {
			ready := flow.completeDepGraphAction(r.ActionID, "trial shutdown action completion")
			scopeCtrl.completeSubtree(ready)
		}
	case scopeTrialOutcomeExtend:
		flow.markFinished(current)
		scopeCtrl.rehomeBlockedRetryWork(ctx, r, trialScopeKey)
		flow.holdActionUnderScope(ctx, watch, current, r, trialScopeKey)
		scopeCtrl.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
		flow.recordError(decision, r)
	case scopeTrialOutcomeRearmOrDiscard:
		flow.markFinished(current)
		scopeCtrl.applyTrialReclassification(ctx, watch, decision, r, bl)
		flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r))
		scopeCtrl.rearmOrDiscardScope(ctx, watch, trialScopeKey)
		flow.recordError(decision, r)
	case scopeTrialOutcomeFatal:
		flow.markFinished(current)
		if current != nil {
			ready := flow.completeDepGraphAction(r.ActionID, "trial fatal action completion")
			scopeCtrl.completeSubtree(ready)
		}
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	}

	return outcome
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

func (flow *engineFlow) readyAfterSuccess(
	ctx context.Context,
	watch *watchRuntime,
	actionID int64,
) []*TrackedAction {
	ready := flow.completeDepGraphAction(actionID, "successful action completion")
	return flow.scopeController().admitReady(ctx, watch, ready)
}

func (flow *engineFlow) holdActionFromPersistedRetryState(
	current *TrackedAction,
	work RetryWorkKey,
) {
	if current == nil {
		return
	}

	row, ok := flow.retryRowsByKey[work]
	if !ok {
		return
	}
	if row.Blocked {
		flow.holdAction(current, heldReasonScope, row.ScopeKey, time.Time{})
		return
	}

	nextRetry := time.Time{}
	if row.NextRetryAt > 0 {
		nextRetry = time.Unix(0, row.NextRetryAt)
	}
	flow.holdAction(current, heldReasonRetry, ScopeKey{}, nextRetry)
}

func (flow *engineFlow) holdActionUnderScope(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	scopeKey ScopeKey,
) {
	if current == nil {
		return
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
		flow.engine.logger.Warn("failed to record blocked retry_work",
			slog.String("path", r.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if row != nil {
		flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
	}
	flow.holdAction(current, heldReasonScope, scopeKey, time.Time{})
	if watch != nil {
		watch.armHeldTimers()
	}
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
	watch *watchRuntime,
) []*TrackedAction {
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
		return nil
	}

	return flow.scopeController().admitReady(ctx, watch, ready)
}

func (controller *scopeController) clearBlockedRetryWorkForScope(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) {
	if scopeKey.IsZero() {
		return
	}

	flow := controller.flow
	if err := flow.engine.baseline.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		flow.engine.logger.Warn("failed to clear stale trial candidate",
			slog.String("path", work.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
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
) bool {
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
		fields := append(flow.resultLogFields(decision, r), slog.String("error", recErr.Error()))
		flow.engine.logger.Warn("failed to record retry_work", fields...)

		return false
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("condition_type", decision.ConditionType),
		slog.String("error", r.ErrMsg),
		slog.String("scope_evidence", scopeKey.String()),
		slog.Bool("blocked", blocked),
	)
	if row != nil {
		flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
		fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	}
	flow.engine.logger.Debug("retry_work recorded", fields...)

	return true
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
