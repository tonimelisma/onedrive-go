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

// processWorkerResult replaces processWorkerResult + routeReadyActions with
// failure-aware dependent dispatch.
func (flow *engineFlow) processWorkerResult(
	ctx context.Context,
	watch *watchRuntime,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
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
) []*synctypes.TrackedAction {
	decision := classifyResult(r)
	ready, _ := flow.depGraph.Complete(r.ActionID)

	if resultCtx.isTrial {
		return flow.processTrialDecision(ctx, watch, resultCtx.trialScopeKey, decision, ready, r)
	}

	return flow.processNormalDecision(ctx, watch, decision, ready, r, bl)
}

// applyResultDecision handles per-class side effects after dependent routing.
func (flow *engineFlow) applyResultDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) {
	switch decision.Class {
	case resultSuccess:
		if decision.RecordSuccess {
			flow.succeeded++
			flow.clearFailureOnSuccess(ctx, r)
			if watch != nil {
				watch.scopeState.RecordSuccess(r)
			}
		}
	case resultSkip, resultShutdown, resultRequeue, resultScopeBlock, resultFatal:
	}

	if flow.applyPermissionDecisionFlow(ctx, watch, decision, r, bl) {
		flow.recordError(r)
		return
	}

	flow.applyFailureRecordMode(ctx, decision.RecordMode, r)

	if decision.RunScopeDetection {
		flow.feedScopeDetection(ctx, watch, r)
	} else if decision.Class == resultScopeBlock && !decision.ScopeKey.IsZero() {
		flow.applyScopeBlock(ctx, watch, synctypes.ScopeUpdateResult{
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

	if decision.Class != resultShutdown && decision.Class != resultSuccess {
		flow.recordError(r)
	}
}

// processTrialResult handles trial results using the engine-owned result flow.
func (flow *engineFlow) processTrialResult(ctx context.Context, watch *watchRuntime, r *synctypes.WorkerResult) {
	flow.processResult(ctx, watch, resultContext{
		isTrial:       true,
		trialScopeKey: r.TrialScopeKey,
	}, r, nil)
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
	var dispatched []*synctypes.TrackedAction

	switch decision.Class {
	case resultSuccess:
		dispatched = flow.admitReady(ctx, watch, ready)
	case resultShutdown:
		flow.completeSubtree(ready)
	case resultRequeue, resultScopeBlock, resultSkip, resultFatal:
		flow.cascadeFailAndComplete(ctx, ready, r)
	}

	flow.applyResultDecision(ctx, watch, decision, r, bl)

	return dispatched
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey synctypes.ScopeKey,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
) []*synctypes.TrackedAction {
	if decision.Class == resultSuccess {
		if err := flow.releaseScope(ctx, watch, trialScopeKey); err != nil {
			flow.engine.logger.Warn("processTrialResult: failed to release scope",
				slog.String("scope_key", trialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		flow.succeeded++
		if watch != nil {
			watch.scopeState.RecordSuccess(r)
		}
		return flow.admitReady(ctx, watch, ready)
	}

	if decision.Class == resultShutdown {
		flow.completeSubtree(ready)
		return nil
	}

	flow.extendScopeTrial(ctx, watch, trialScopeKey, r.RetryAfter)
	flow.applyFailureRecordMode(ctx, decision.RecordMode, r)
	flow.recordError(r)
	flow.cascadeFailAndComplete(ctx, ready, r)
	if watch != nil {
		watch.armRetryTimer(ctx)
	}

	return nil
}

func (flow *engineFlow) applyPermissionDecisionFlow(
	ctx context.Context,
	watch *watchRuntime,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) bool {
	switch decision.PermissionFlow {
	case permissionFlowNone:
		return false
	case permissionFlowRemote403:
		if !flow.engine.permHandler.HasPermChecker() {
			return false
		}
		decision := flow.engine.permHandler.handle403(ctx, bl, r.Path, flow.getShortcuts())
		return flow.applyPermissionCheckDecision(
			ctx,
			watch,
			permissionFlowRemote403,
			&decision,
		)
	case permissionFlowLocalPermission:
		decision := flow.engine.permHandler.handleLocalPermission(ctx, r)
		return flow.applyPermissionCheckDecision(
			ctx,
			watch,
			permissionFlowLocalPermission,
			&decision,
		)
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
