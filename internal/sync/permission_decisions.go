package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func (controller *scopeController) applyPermissionOutcome(
	ctx context.Context,
	watch *watchRuntime,
	flowKind permissionFlow,
	outcome *PermissionOutcome,
) bool {
	if outcome == nil || !outcome.Matched {
		return false
	}

	controller.applyPermissionOutcomeMutation(ctx, watch, outcome)
	controller.logPermissionOutcome(flowKind, outcome)

	return true
}

func (controller *scopeController) applyPermissionOutcomeMutation(
	ctx context.Context,
	watch *watchRuntime,
	outcome *PermissionOutcome,
) {
	switch outcome.Kind {
	case permissionOutcomeNone:
		return
	case permissionOutcomeRecordFileFailure,
		permissionOutcomeActivateBoundaryScope,
		permissionOutcomeActivateDerivedScope:
		if !controller.recordRetryWorkFailure(ctx, outcome.Kind, outcome.RetryWorkFailure) {
			return
		}
		if watch != nil && shouldArmPermissionRetryTimer(outcome) {
			watch.armRetryTimer()
		}
		if !outcome.ScopeKey.IsZero() && outcome.ScopeKey.PersistsInBlockScopes() {
			controller.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      outcome.ScopeKey,
				ConditionType: outcome.ScopeKey.ConditionType(),
			})
		}
	default:
		panic(fmt.Sprintf("unknown permission outcome kind %d", outcome.Kind))
	}
}

func (controller *scopeController) logPermissionOutcome(
	flowKind permissionFlow,
	outcome *PermissionOutcome,
) {
	conditionKey := outcome.ConditionKey()

	switch flowKind {
	case permissionFlowNone:
		return
	case permissionFlowRemote403:
		controller.logRemotePermissionOutcome(outcome, conditionKey)
	case permissionFlowLocalPermission:
		if outcome.Kind == permissionOutcomeActivateBoundaryScope {
			fields := append(controller.flow.summaryLogFields(
				errclass.ClassActionable,
				conditionKey,
				outcome.TriggerPath,
				outcome.ScopeKey,
			),
				slog.String("boundary", outcome.BoundaryPath),
				slog.String("trigger_path", outcome.TriggerPath),
			)
			controller.flow.engine.logger.Info("local permission denied: directory blocked", fields...)
		}
	default:
		panic(fmt.Sprintf("unknown permission flow %d", flowKind))
	}
}

func (controller *scopeController) logRemotePermissionOutcome(
	outcome *PermissionOutcome,
	conditionKey ConditionKey,
) {
	if outcome == nil || !outcome.IsBoundaryFailure() {
		return
	}

	scopeKey := outcome.ScopeKey
	fields := append(controller.flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKey,
		outcome.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", outcome.BoundaryPath),
		slog.String("trigger_path", outcome.TriggerPath),
	)
	controller.flow.engine.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (controller *scopeController) recordRetryWorkFailure(
	ctx context.Context,
	kind PermissionOutcomeKind,
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

	delayFn := permissionOutcomeRetryDelay(kind, failure)
	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, failure, delayFn)
	if err != nil {
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
	if row != nil {
		flow.retryRowsByKey[retryWorkKeyForRetryWork(row)] = *row
	}
	flow.engine.logger.Debug("retry_work permission failure recorded", fields...)

	return true
}

func permissionOutcomeRetryDelay(
	kind PermissionOutcomeKind,
	failure *RetryWorkFailure,
) func(int) time.Duration {
	if kind != permissionOutcomeRecordFileFailure || failure == nil || failure.Blocked {
		return nil
	}

	return retry.ReconcilePolicy().Delay
}

func shouldArmPermissionRetryTimer(outcome *PermissionOutcome) bool {
	if outcome == nil || !outcome.IsFileFailure() {
		return false
	}

	return outcome.RetryWorkFailure != nil && !outcome.RetryWorkFailure.Blocked
}
