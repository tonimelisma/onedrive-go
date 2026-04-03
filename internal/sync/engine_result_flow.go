package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type resultContext struct {
	isTrial       bool
	trialScopeKey synctypes.ScopeKey
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
	dispatched   []*synctypes.TrackedAction
	terminate    bool
	terminateErr error
}

// processWorkerResult replaces processWorkerResult + routeReadyActions with
// failure-aware dependent dispatch.
func (flow *engineFlow) processWorkerResult(
	ctx context.Context,
	watch *watchRuntime,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
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
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) routeOutcome {
	decision := classifyResult(r)
	ready, _ := flow.depGraph.Complete(r.ActionID)

	if resultCtx.isTrial {
		return flow.processTrialDecision(ctx, watch, resultCtx.trialScopeKey, decision, ready, r, bl)
	}

	return flow.processNormalDecision(ctx, watch, decision, ready, r, bl)
}

func (flow *engineFlow) routeReadyForClass(
	ctx context.Context,
	watch *watchRuntime,
	class resultClass,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
) []*synctypes.TrackedAction {
	switch class {
	case resultSuccess:
		return flow.scopeController().admitReady(ctx, watch, ready)
	case resultShutdown:
		flow.scopeController().completeSubtree(ready)
	case resultFatal:
		flow.scopeController().completeSubtree(ready)
	case resultRequeue, resultScopeBlock, resultSkip:
		flow.scopeController().cascadeFailAndComplete(ctx, ready, r)
	}

	return nil
}

func (flow *engineFlow) applySuccessEffects(ctx context.Context, watch *watchRuntime, r *synctypes.WorkerResult) {
	flow.succeeded++
	flow.clearFailureOnSuccess(ctx, r)
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
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) {
	if flow.scopeController().applyPermissionDecisionFlow(ctx, watch, decision, r, bl) {
		flow.recordError(r)
		return
	}

	flow.applyFailureRecordMode(ctx, decision.RecordMode, r)

	if decision.RunScopeDetection {
		flow.scopeController().feedScopeDetection(ctx, watch, r)
	} else if decision.Class == resultScopeBlock && !decision.ScopeKey.IsZero() {
		flow.scopeController().applyScopeBlock(ctx, watch, synctypes.ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
	}

	if decision.Class == resultScopeBlock && watch != nil {
		watch.armTrialTimer()
	}
	if decision.RecordMode == recordFailureReconcile && watch != nil {
		watch.armRetryTimer(ctx)
	}

	flow.recordError(r)
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
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

				flow.recordError(r)

				return outcome
			}

			decision.PermissionFlow = permissionFlowNone
		}
	}

	outcome := routeOutcome{
		dispatched: flow.routeReadyForClass(ctx, watch, decision.Class, ready, r),
	}

	switch decision.Class {
	case resultSuccess:
		flow.applySuccessEffects(ctx, watch, r)
	case resultShutdown:
		return outcome
	case resultFatal:
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r)
		flow.recordError(r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	case resultRequeue, resultScopeBlock, resultSkip:
		flow.applyOrdinaryFailureEffects(ctx, watch, decision, r, bl)
	}

	return outcome
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey synctypes.ScopeKey,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) routeOutcome {
	scopeCtrl := flow.scopeController()
	outcome := routeOutcome{}

	switch flow.evaluateTrialOutcome(trialScopeKey, decision, r) {
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
		flow.routeReadyForClass(ctx, watch, resultShutdown, ready, r)
	case trialOutcomeExtend:
		flow.routeReadyForClass(ctx, watch, resultRequeue, ready, r)
		scopeCtrl.rehomeHeldFailure(ctx, r, trialScopeKey)
		scopeCtrl.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
		flow.recordError(r)
	case trialOutcomePreserve:
		flow.routeReadyForClass(ctx, watch, resultRequeue, ready, r)
		scopeCtrl.preserveScopeTrial(ctx, watch, trialScopeKey)
		scopeCtrl.applyTrialPreserveEffects(ctx, watch, decision, r, bl)
		flow.recordError(r)
	case trialOutcomeFatal:
		flow.routeReadyForClass(ctx, watch, resultFatal, ready, r)
		scopeCtrl.applyFatalAuthEffects(ctx, watch, r)
		flow.recordError(r)
		outcome.terminate = true
		outcome.terminateErr = fatalResultError(r)
	}

	return outcome
}

func (flow *engineFlow) evaluateTrialOutcome(
	trialScopeKey synctypes.ScopeKey,
	decision ResultDecision,
	r *synctypes.WorkerResult,
) trialOutcome {
	switch decision.Class {
	case resultSuccess:
		return trialOutcomeRelease
	case resultRequeue, resultScopeBlock, resultSkip:
		if flow.trialScopePersists(trialScopeKey, decision, r) {
			return trialOutcomeExtend
		}
		return trialOutcomePreserve
	case resultShutdown:
		return trialOutcomeShutdown
	case resultFatal:
		return trialOutcomeFatal
	}

	panic(fmt.Sprintf("unknown result class %d", decision.Class))
}

func (flow *engineFlow) trialScopePersists(
	trialScopeKey synctypes.ScopeKey,
	decision ResultDecision,
	r *synctypes.WorkerResult,
) bool {
	switch trialScopeKey {
	case synctypes.SKThrottleAccount():
		return decision.Class == resultScopeBlock && decision.ScopeKey == synctypes.SKThrottleAccount()
	case synctypes.SKService():
		return r.HTTPStatus >= 500 && r.HTTPStatus < 600
	case synctypes.SKDiskLocal():
		return decision.Class == resultScopeBlock && decision.ScopeKey == synctypes.SKDiskLocal()
	default:
		return decision.Class == resultScopeBlock && decision.ScopeKey == trialScopeKey
	}
}

func (controller *scopeController) applyTrialPreserveEffects(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) {
	if decision.PermissionFlow != permissionFlowNone {
		if permDecision, handled := controller.resolvePermissionDecision(ctx, decision, r, bl); handled {
			controller.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
		}
		return
	}

	if decision.Class == resultScopeBlock && decision.ScopeKey == synctypes.SKDiskLocal() {
		controller.applyScopeBlock(ctx, watch, synctypes.ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
		controller.rehomeHeldFailure(ctx, r, decision.ScopeKey)
	}
}

func fatalResultError(r *synctypes.WorkerResult) error {
	if r.Err != nil {
		return fmt.Errorf("sync: unauthorized worker result for %s: %w", r.Path, r.Err)
	}

	return fmt.Errorf("sync: unauthorized worker result for %s", r.Path)
}

func (controller *scopeController) applyFatalAuthEffects(
	ctx context.Context,
	watch *watchRuntime,
	r *synctypes.WorkerResult,
) {
	flow := controller.flow

	if err := controller.activateAuthScope(ctx, watch); err != nil {
		flow.engine.logger.Warn("fatal unauthorized: failed to persist auth scope",
			slog.String("path", r.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (controller *scopeController) applyPermissionDecisionFlow(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) bool {
	permDecision, handled := controller.resolvePermissionDecision(ctx, decision, r, bl)
	if !handled {
		return false
	}

	return controller.applyPermissionCheckDecision(ctx, watch, decision.PermissionFlow, permDecision)
}

func (controller *scopeController) resolvePermissionDecision(
	ctx context.Context,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) (*PermissionCheckDecision, bool) {
	flow := controller.flow

	switch decision.PermissionFlow {
	case permissionFlowNone:
		return nil, false
	case permissionFlowRemote403:
		if bl == nil || !flow.engine.permHandler.HasPermChecker() {
			return nil, false
		}
		permDecision := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType, flow.getShortcuts())
		return &permDecision, true
	case permissionFlowLocalPermission:
		permDecision := flow.engine.permHandler.handleLocalPermission(ctx, r)
		return &permDecision, true
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
}

// recordFailure writes a failure to sync_failures with the given delay
// function for computing next_retry_at.
func (flow *engineFlow) recordFailure(ctx context.Context, r *synctypes.WorkerResult, delayFn func(int) time.Duration) {
	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	category := synctypes.CategoryTransient
	if delayFn == nil {
		category = synctypes.CategoryActionable
	}

	issueType := issueTypeForHTTPStatus(r.HTTPStatus, r.Err)
	scopeKey := deriveScopeKey(r)

	if recErr := flow.engine.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       r.Path,
		DriveID:    driveID,
		Direction:  direction,
		ActionType: r.ActionType,
		Role:       synctypes.FailureRoleItem,
		Category:   category,
		IssueType:  issueType,
		ErrMsg:     r.ErrMsg,
		HTTPStatus: r.HTTPStatus,
		ScopeKey:   scopeKey,
	}, delayFn); recErr != nil {
		flow.engine.logger.Warn("failed to record failure",
			slog.String("path", r.Path),
			slog.String("error", recErr.Error()),
		)

		return
	}

	flow.engine.logger.Debug("sync failure recorded",
		slog.String("path", r.Path),
		slog.String("action", r.ActionType.String()),
		slog.Int("http_status", r.HTTPStatus),
		slog.String("error", r.ErrMsg),
		slog.String("scope_key", scopeKey.String()),
	)
}
