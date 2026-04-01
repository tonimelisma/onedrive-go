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
func (e *Engine) processWorkerResult(ctx context.Context, r *synctypes.WorkerResult, bl *synctypes.Baseline) []*synctypes.TrackedAction {
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		return e.processResult(ctx, resultContext{
			isTrial:       true,
			trialScopeKey: r.TrialScopeKey,
		}, r, bl)
	}

	return e.processResult(ctx, resultContext{}, r, bl)
}

func (e *Engine) processResult(
	ctx context.Context,
	resultCtx resultContext,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
	decision := classifyResult(r)
	ready, _ := e.depGraph.Complete(r.ActionID)

	if resultCtx.isTrial {
		return e.processTrialDecision(ctx, resultCtx.trialScopeKey, decision, ready, r)
	}

	return e.processNormalDecision(ctx, decision, ready, r, bl)
}

// applyResultDecision handles per-class side effects after dependent routing.
func (e *Engine) applyResultDecision(ctx context.Context, decision ResultDecision, r *synctypes.WorkerResult, bl *synctypes.Baseline) {
	switch decision.Class {
	case resultSuccess:
		if decision.RecordSuccess {
			e.succeeded++
			e.clearFailureOnSuccess(ctx, r)
			if e.watch != nil {
				e.watch.scopeState.RecordSuccess(r)
			}
		}
	case resultSkip, resultShutdown, resultRequeue, resultScopeBlock, resultFatal:
	}

	if e.applyPermissionDecisionFlow(ctx, decision, r, bl) {
		e.recordError(r)
		return
	}

	e.applyFailureRecordMode(ctx, decision.RecordMode, r)

	if decision.RunScopeDetection {
		e.feedScopeDetection(ctx, r)
	} else if decision.Class == resultScopeBlock && !decision.ScopeKey.IsZero() {
		e.applyScopeBlock(ctx, synctypes.ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
	}

	if decision.Class == resultScopeBlock {
		e.armTrialTimer()
	}
	if decision.RecordMode == recordFailureReconcile {
		e.armRetryTimer(ctx)
	}

	if decision.Class != resultShutdown && decision.Class != resultSuccess {
		e.recordError(r)
	}
}

// processTrialResult handles trial results using the engine-owned result flow.
func (e *Engine) processTrialResult(ctx context.Context, r *synctypes.WorkerResult) {
	e.processResult(ctx, resultContext{
		isTrial:       true,
		trialScopeKey: r.TrialScopeKey,
	}, r, nil)
}

func (e *Engine) processNormalDecision(
	ctx context.Context,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
	var dispatched []*synctypes.TrackedAction

	switch decision.Class {
	case resultSuccess:
		dispatched = e.admitReady(ctx, ready)
	case resultShutdown:
		e.completeSubtree(ready)
	case resultRequeue, resultScopeBlock, resultSkip, resultFatal:
		e.cascadeFailAndComplete(ctx, ready, r)
	}

	e.applyResultDecision(ctx, decision, r, bl)

	return dispatched
}

func (e *Engine) processTrialDecision(
	ctx context.Context,
	trialScopeKey synctypes.ScopeKey,
	decision ResultDecision,
	ready []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
) []*synctypes.TrackedAction {
	if decision.Class == resultSuccess {
		if err := e.releaseScope(ctx, trialScopeKey); err != nil {
			e.logger.Warn("processTrialResult: failed to release scope",
				slog.String("scope_key", trialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		e.succeeded++
		if e.watch != nil {
			e.watch.scopeState.RecordSuccess(r)
		}
		return e.admitReady(ctx, ready)
	}

	if decision.Class == resultShutdown {
		e.completeSubtree(ready)
		return nil
	}

	e.extendScopeTrial(ctx, trialScopeKey, r.RetryAfter)
	e.applyFailureRecordMode(ctx, decision.RecordMode, r)
	e.recordError(r)
	e.cascadeFailAndComplete(ctx, ready, r)
	e.armRetryTimer(ctx)

	return nil
}

func (e *Engine) applyPermissionDecisionFlow(
	ctx context.Context,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) bool {
	switch decision.PermissionFlow {
	case permissionFlowNone:
		return false
	case permissionFlowRemote403:
		if !e.permHandler.HasPermChecker() {
			return false
		}
		decision := e.permHandler.handle403(ctx, bl, r.Path, e.getShortcuts())
		return e.applyPermissionCheckDecision(
			ctx,
			permissionFlowRemote403,
			&decision,
		)
	case permissionFlowLocalPermission:
		decision := e.permHandler.handleLocalPermission(ctx, r)
		return e.applyPermissionCheckDecision(
			ctx,
			permissionFlowLocalPermission,
			&decision,
		)
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
}

// recordFailure writes a failure to sync_failures with the given delay
// function for computing next_retry_at.
func (e *Engine) recordFailure(ctx context.Context, r *synctypes.WorkerResult, delayFn func(int) time.Duration) {
	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	category := synctypes.CategoryTransient
	if delayFn == nil {
		category = synctypes.CategoryActionable
	}

	issueType := issueTypeForHTTPStatus(r.HTTPStatus, r.Err)
	scopeKey := deriveScopeKey(r)

	if recErr := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
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
		e.logger.Warn("failed to record failure",
			slog.String("path", r.Path),
			slog.String("error", recErr.Error()),
		)

		return
	}

	e.logger.Debug("sync failure recorded",
		slog.String("path", r.Path),
		slog.String("action", r.ActionType.String()),
		slog.Int("http_status", r.HTTPStatus),
		slog.String("error", r.ErrMsg),
		slog.String("scope_key", scopeKey.String()),
	)
}
