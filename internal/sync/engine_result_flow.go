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

type trialOutcome int

const (
	trialOutcomeRelease trialOutcome = iota
	trialOutcomeExtend
	trialOutcomePreserve
	trialOutcomeShutdown
	trialOutcomeFatal
)

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
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		return flow.processResult(ctx, watch, resultContext{
			isTrial:       true,
			trialScopeKey: r.TrialScopeKey,
		}, r, bl)
	}

	return flow.processResult(ctx, watch, resultContext{}, r, bl)
}

func (flow *engineFlow) processResult(
	ctx context.Context,
	watch *watchRuntime,
	resultCtx resultContext,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	decision := classifyResult(r)
	ready := flow.completeDepGraphAction(r.ActionID, "processResult")

	if resultCtx.isTrial {
		return flow.processTrialDecision(ctx, watch, resultCtx.trialScopeKey, &decision, ready, r, bl)
	}

	return flow.processNormalDecision(ctx, watch, &decision, ready, r, bl)
}

func (flow *engineFlow) routeReadyForClass(
	ctx context.Context,
	watch *watchRuntime,
	class errclass.Class,
	ready []*TrackedAction,
	r *ActionCompletion,
) []*TrackedAction {
	switch class {
	case errclass.ClassInvalid:
		flow.scopeController().cascadeFailAndComplete(ctx, ready, r)
	case errclass.ClassSuccess:
		return flow.scopeController().admitReady(ctx, watch, ready)
	case errclass.ClassShutdown:
		flow.scopeController().completeSubtree(ready)
	case errclass.ClassFatal:
		flow.scopeController().completeSubtree(ready)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		flow.scopeController().cascadeFailAndComplete(ctx, ready, r)
	}

	return nil
}

func (flow *engineFlow) applySuccessEffects(ctx context.Context, watch *watchRuntime, r *ActionCompletion) {
	flow.succeeded++
	flow.clearRetryWorkOnSuccess(ctx, r)
	if watch != nil {
		watch.scopeState.RecordSuccess(r)
	}
}

// applyOrdinaryFailureEffects handles post-routing side effects for normal
// worker failures. Trial results intentionally use separate scope-relative
// policy so they do not accidentally mutate the original scope via generic
// failure recording or scope detection.
func (flow *engineFlow) applyOrdinaryFailureEffects(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) {
	if flow.scopeController().applyPermissionDecisionFlow(ctx, watch, decision, r, bl) {
		flow.recordError(decision, r)
		return
	}

	flow.applyResultPersistence(ctx, decision, r)

	if decision.RunScopeDetection {
		flow.scopeController().feedScopeDetection(ctx, watch, r)
	} else if decision.Class == errclass.ClassBlockScopeingTransient && !decision.ScopeKey.IsZero() {
		flow.scopeController().applyBlockScope(ctx, watch, ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
	}

	if decision.Class == errclass.ClassBlockScopeingTransient && watch != nil {
		watch.armTrialTimer()
	}
	if decision.Persistence == persistRetryWork && watch != nil {
		watch.armRetryTimer(ctx)
	}

	flow.recordError(decision, r)
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	ready []*TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	scopeCtrl := flow.scopeController()

	if decision.PermissionFlow != permissionFlowNone {
		if permDecision, handled := scopeCtrl.resolvePermissionDecision(ctx, decision, r, bl); handled {
			if permDecision.Matched {
				outcome := routeOutcome{}
				switch permDecision.Kind {
				case permissionCheckNone,
					permissionCheckRecordFileFailure,
					permissionCheckActivateBoundaryScope:
					outcome.dispatched = flow.routeReadyForClass(ctx, watch, decision.Class, ready, r)
					scopeCtrl.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
				case permissionCheckActivateDerivedScope:
					scopeCtrl.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
					for _, ta := range ready {
						scopeCtrl.cascadeRecordAndComplete(ctx, ta, permDecision.ScopeKey)
					}
				default:
					panic(fmt.Sprintf("unknown permission check decision kind %d", permDecision.Kind))
				}

				flow.recordError(decision, r)

				return outcome
			}

			decision.PermissionFlow = permissionFlowNone
		}
	}

	outcome := routeOutcome{
		dispatched: flow.routeReadyForClass(ctx, watch, decision.Class, ready, r),
	}

	switch decision.Class {
	case errclass.ClassInvalid:
		flow.applyOrdinaryFailureEffects(ctx, watch, decision, r, bl)
		outcome.terminate = true
		outcome.terminateErr = fmt.Errorf("classify action completion: invalid failure class")
	case errclass.ClassSuccess:
		flow.applySuccessEffects(ctx, watch, r)
	case errclass.ClassShutdown:
		return outcome
	case errclass.ClassFatal:
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.SummaryKey)
		flow.recordError(decision, r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		flow.applyOrdinaryFailureEffects(ctx, watch, decision, r, bl)
	}

	return outcome
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey ScopeKey,
	decision *ResultDecision,
	ready []*TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) routeOutcome {
	scopeCtrl := flow.scopeController()
	outcome := routeOutcome{}

	switch flow.evaluateTrialOutcome(trialScopeKey, decision) {
	case trialOutcomeRelease:
		flow.applySuccessEffects(ctx, watch, r)
		if err := scopeCtrl.releaseScope(ctx, watch, trialScopeKey); err != nil {
			flow.engine.logger.Warn("trial result: failed to release scope",
				slog.String("scope_key", trialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		outcome.dispatched = flow.routeReadyForClass(ctx, watch, resultSuccess, ready, r)
	case trialOutcomeShutdown:
		flow.routeReadyForClass(ctx, watch, errclass.ClassShutdown, ready, r)
	case trialOutcomeExtend:
		flow.routeReadyForClass(ctx, watch, decision.Class, ready, r)
		scopeCtrl.rehomeBlockedRetryWork(ctx, r, trialScopeKey)
		scopeCtrl.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
		flow.recordError(decision, r)
	case trialOutcomePreserve:
		flow.routeReadyForClass(ctx, watch, decision.Class, ready, r)
		scopeCtrl.preserveScopeTrial(ctx, watch, trialScopeKey)
		scopeCtrl.applyTrialPreserveEffects(ctx, watch, decision, r, bl)
		flow.recordError(decision, r)
	case trialOutcomeFatal:
		flow.routeReadyForClass(ctx, watch, errclass.ClassFatal, ready, r)
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r, decision.SummaryKey)
		flow.recordError(decision, r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	}

	return outcome
}

func (flow *engineFlow) evaluateTrialOutcome(
	trialScopeKey ScopeKey,
	decision *ResultDecision,
) trialOutcome {
	switch decision.TrialHint {
	case trialHintRelease:
		return trialOutcomeRelease
	case trialHintExtendOnMatchingScope:
		if flow.trialScopePersists(trialScopeKey, decision) {
			return trialOutcomeExtend
		}
		return trialOutcomePreserve
	case trialHintPreserve:
		return trialOutcomePreserve
	case trialHintShutdown:
		return trialOutcomeShutdown
	case trialHintFatal:
		return trialOutcomeFatal
	}

	panic(fmt.Sprintf("unknown trial hint %d", decision.TrialHint))
}

func (flow *engineFlow) trialScopePersists(
	trialScopeKey ScopeKey,
	decision *ResultDecision,
) bool {
	return !decision.ScopeEvidence.IsZero() && decision.ScopeEvidence == trialScopeKey
}

func (controller *scopeController) applyTrialPreserveEffects(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) {
	if decision.PermissionFlow != permissionFlowNone {
		if permDecision, handled := controller.resolvePermissionDecision(ctx, decision, r, bl); handled {
			controller.clearBlockedRetryWorkForScope(ctx, retryWorkKeyForCompletion(r), r.TrialScopeKey)
			controller.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
		}
		return
	}

	if decision.Class == errclass.ClassBlockScopeingTransient && decision.ScopeKey == SKDiskLocal() {
		controller.applyBlockScope(ctx, watch, ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
		controller.rehomeBlockedRetryWork(ctx, r, decision.ScopeKey)
	}
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
		flow.engine.logger.Warn("failed to clear preserved trial candidate",
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
	summaryKey SummaryKey,
) {
	flow := controller.flow
	logFields := flow.summaryLogFields(
		errclass.ClassFatal,
		summaryKey,
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

func (controller *scopeController) applyPermissionDecisionFlow(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) bool {
	permDecision, handled := controller.resolvePermissionDecision(ctx, decision, r, bl)
	if !handled {
		return false
	}

	return controller.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
}

func (controller *scopeController) resolvePermissionDecision(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) (*PermissionCheckDecision, bool) {
	flow := controller.flow

	switch decision.PermissionFlow {
	case permissionFlowNone:
		return nil, false
	case permissionFlowRemote403:
		if bl == nil || !flow.engine.permHandler.HasPermChecker() {
			return nil, false
		}
		permDecision := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType)
		return &permDecision, true
	case permissionFlowLocalPermission:
		permDecision := flow.engine.permHandler.handleLocalPermission(ctx, r)
		return &permDecision, true
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
}

func (flow *engineFlow) recordObservationIssue(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
) {
	driveID := r.DriveID
	if driveID.IsZero() {
		driveID = flow.engine.driveID
	}

	issue := &ObservationIssue{
		Path:       r.Path,
		DriveID:    driveID,
		ActionType: r.ActionType,
		IssueType:  decision.IssueType,
		Error:      r.ErrMsg,
		ScopeKey:   decision.ScopeEvidence,
	}

	if recErr := flow.engine.baseline.UpsertObservationIssue(ctx, issue); recErr != nil {
		fields := append(flow.resultLogFields(decision, r), slog.String("error", recErr.Error()))
		flow.engine.logger.Warn("failed to record observation issue", fields...)
		return
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("issue_type", decision.IssueType),
		slog.String("scope_evidence", decision.ScopeEvidence.String()),
	)
	flow.engine.logger.Debug("observation issue recorded", fields...)
}

func (flow *engineFlow) recordRetryWork(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	delayFn func(int) time.Duration,
) {
	scopeKey := decision.ScopeEvidence
	blocked := !scopeKey.IsZero() &&
		(decision.Class == errclass.ClassRetryableTransient || decision.Class == errclass.ClassBlockScopeingTransient)

	row, recErr := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       r.Path,
		OldPath:    r.OldPath,
		ActionType: r.ActionType,
		IssueType:  decision.IssueType,
		ScopeKey:   scopeKey,
		LastError:  r.ErrMsg,
		HTTPStatus: r.HTTPStatus,
		Blocked:    blocked,
	}, delayFn)
	if recErr != nil {
		fields := append(flow.resultLogFields(decision, r), slog.String("error", recErr.Error()))
		flow.engine.logger.Warn("failed to record retry_work", fields...)

		return
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("issue_type", decision.IssueType),
		slog.String("error", r.ErrMsg),
		slog.String("scope_evidence", scopeKey.String()),
		slog.Bool("blocked", blocked),
	)
	if row != nil {
		fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	}
	flow.engine.logger.Debug("retry_work recorded", fields...)
}
