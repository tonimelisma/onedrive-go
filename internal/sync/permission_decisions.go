package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

type PermissionCheckDecisionKind int

const (
	permissionCheckNone PermissionCheckDecisionKind = iota
	permissionCheckRecordFileFailure
	permissionCheckActivateBoundaryScope
	permissionCheckActivateDerivedScope
)

// PermissionCheckDecision is the policy-layer output for a single permission
// check performed during worker-result handling. Matched=false means the
// engine should fall back to generic result persistence.
type PermissionCheckDecision struct {
	Matched          bool
	Kind             PermissionCheckDecisionKind
	RetryWorkFailure *RetryWorkFailure
	ScopeKey         ScopeKey
	BoundaryPath     string
	TriggerPath      string
}

func (controller *scopeController) applyPermissionCheckDecision(
	ctx context.Context,
	watch *watchRuntime,
	flowKind permissionFlow,
	decision *PermissionCheckDecision,
) bool {
	if decision == nil || !decision.Matched {
		return false
	}

	controller.applyPermissionCheckMutation(ctx, watch, decision)
	controller.logPermissionCheckDecision(flowKind, decision)

	return true
}

func (controller *scopeController) applyPermissionCheckMutation(
	ctx context.Context,
	watch *watchRuntime,
	decision *PermissionCheckDecision,
) {
	switch decision.Kind {
	case permissionCheckNone:
		return
	case permissionCheckRecordFileFailure,
		permissionCheckActivateBoundaryScope,
		permissionCheckActivateDerivedScope:
		if !controller.recordRetryWorkFailure(ctx, decision.Kind, decision.RetryWorkFailure) {
			return
		}
		if !decision.ScopeKey.IsZero() {
			controller.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      decision.ScopeKey,
				ConditionType: decision.ScopeKey.ConditionType(),
			})
		}
	default:
		panic(fmt.Sprintf("unknown permission check decision kind %d", decision.Kind))
	}
}

func (controller *scopeController) logPermissionCheckDecision(
	flowKind permissionFlow,
	decision *PermissionCheckDecision,
) {
	conditionKey := permissionDecisionConditionKey(decision)

	switch flowKind {
	case permissionFlowNone:
		return
	case permissionFlowRemote403:
		controller.logRemotePermissionDecision(decision, conditionKey)
	case permissionFlowLocalPermission:
		if decision.Kind == permissionCheckActivateBoundaryScope {
			fields := append(controller.flow.summaryLogFields(
				errclass.ClassActionable,
				conditionKey,
				decision.TriggerPath,
				decision.ScopeKey,
			),
				slog.String("boundary", decision.BoundaryPath),
				slog.String("trigger_path", decision.TriggerPath),
			)
			controller.flow.engine.logger.Info("local permission denied: directory blocked", fields...)
		}
	default:
		panic(fmt.Sprintf("unknown permission flow %d", flowKind))
	}
}

func permissionDecisionConditionKey(decision *PermissionCheckDecision) ConditionKey {
	if decision == nil {
		return ""
	}

	if !decision.ScopeKey.IsZero() {
		return ConditionKeyForStoredCondition(decision.ScopeKey.ConditionType(), decision.ScopeKey)
	}

	if decision.RetryWorkFailure != nil {
		return ConditionKeyForStoredCondition(
			decision.RetryWorkFailure.ConditionType,
			decision.RetryWorkFailure.ScopeKey,
		)
	}

	if !decision.ScopeKey.IsZero() {
		return ConditionKeyForStoredCondition(decision.ScopeKey.ConditionType(), decision.ScopeKey)
	}

	return ""
}

func (controller *scopeController) logRemotePermissionDecision(
	decision *PermissionCheckDecision,
	conditionKey ConditionKey,
) {
	if decision.Kind != permissionCheckActivateBoundaryScope && decision.Kind != permissionCheckActivateDerivedScope {
		return
	}

	scopeKey := decision.ScopeKey
	fields := append(controller.flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKey,
		decision.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", decision.BoundaryPath),
		slog.String("trigger_path", decision.TriggerPath),
	)
	controller.flow.engine.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (controller *scopeController) recordRetryWorkFailure(
	ctx context.Context,
	kind PermissionCheckDecisionKind,
	failure *RetryWorkFailure,
) bool {
	if failure == nil {
		return false
	}

	flow := controller.flow
	conditionKey := ConditionKeyForStoredCondition(failure.ConditionType, failure.ScopeKey)
	logClass := errclass.ClassActionable
	if failure.Blocked {
		logClass = errclass.ClassBlockScopeingTransient
	}

	delayFn := permissionDecisionRetryDelay(kind, failure)
	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, failure, delayFn); err != nil {
		fields := append(flow.summaryLogFields(
			logClass,
			conditionKey,
			failure.Path,
			failure.ScopeKey,
		), slog.String("error", err.Error()))
		flow.engine.logger.Warn("failed to record retry_work permission failure", fields...)
		return false
	}

	fields := append(flow.summaryLogFields(
		logClass,
		conditionKey,
		failure.Path,
		failure.ScopeKey,
	),
		slog.String("condition_type", failure.ConditionType),
	)
	flow.engine.logger.Debug("retry_work permission failure recorded", fields...)

	return true
}

func permissionDecisionRetryDelay(
	kind PermissionCheckDecisionKind,
	failure *RetryWorkFailure,
) func(int) time.Duration {
	if kind != permissionCheckRecordFileFailure || failure == nil || failure.Blocked {
		return nil
	}

	return retry.ReconcilePolicy().Delay
}
