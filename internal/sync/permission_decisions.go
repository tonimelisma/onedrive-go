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
) (bool, error) {
	if outcome == nil || !outcome.Matched {
		return false, nil
	}

	if err := controller.applyPermissionOutcomeMutation(ctx, watch, outcome); err != nil {
		return true, err
	}
	controller.logPermissionOutcome(flowKind, outcome)

	return true, nil
}

func (controller *scopeController) applyPermissionOutcomeMutation(
	ctx context.Context,
	watch *watchRuntime,
	outcome *PermissionOutcome,
) error {
	switch outcome.Kind {
	case permissionOutcomeNone:
		return nil
	case permissionOutcomeRecordFileFailure,
		permissionOutcomeActivateBoundaryScope,
		permissionOutcomeActivateDerivedScope:
		if err := controller.recordRetryWorkFailure(ctx, outcome.Kind, outcome.RetryWorkFailure); err != nil {
			return err
		}
		if watch != nil && shouldArmPermissionRetryTimer(outcome) {
			watch.armRetryTimer()
		}
		if !outcome.ScopeKey.IsZero() && outcome.ScopeKey.PersistsInBlockScopes() {
			if err := controller.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      outcome.ScopeKey,
				ConditionType: outcome.ScopeKey.ConditionType(),
			}); err != nil {
				return err
			}
		}
		return nil
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
) error {
	if failure == nil {
		return nil
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
		return fmt.Errorf("record permission retry_work for %s: %w", failure.Path, err)
	}

	fields := append(flow.summaryLogFields(
		logClass,
		conditionKey,
		failure.Path,
		failure.ScopeKey,
	),
		slog.String("condition_type", failure.ConditionType),
	)
	if row == nil {
		return fmt.Errorf("record permission retry_work for %s: missing persisted row", failure.Path)
	}
	flow.retryRowsByKey[retryWorkKeyForRetryWork(row)] = *row
	flow.engine.logger.Debug("retry_work permission failure recorded", fields...)

	return nil
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
