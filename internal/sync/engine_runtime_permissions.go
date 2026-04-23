package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func (flow *engineFlow) applyPermissionOutcome(
	ctx context.Context,
	watch *watchRuntime,
	flowKind permissionFlow,
	outcome *PermissionOutcome,
) (bool, error) {
	if outcome == nil || !outcome.Matched {
		return false, nil
	}

	if err := flow.applyPermissionOutcomeMutation(ctx, watch, outcome); err != nil {
		return true, err
	}
	flow.logPermissionOutcome(flowKind, outcome)

	return true, nil
}

func (flow *engineFlow) applyPermissionOutcomeMutation(
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
		if err := flow.recordRetryWorkFailure(ctx, outcome.Kind, outcome.RetryWorkFailure); err != nil {
			return err
		}
		if watch != nil && shouldArmPermissionRetryTimer(outcome) {
			watch.armRetryTimer()
		}
		if !outcome.ScopeKey.IsZero() && outcome.ScopeKey.PersistsInBlockScopes() {
			if err := flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
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

func (flow *engineFlow) logPermissionOutcome(
	flowKind permissionFlow,
	outcome *PermissionOutcome,
) {
	conditionKey := outcome.ConditionKey()

	switch flowKind {
	case permissionFlowNone:
		return
	case permissionFlowRemote403:
		flow.logRemotePermissionOutcome(outcome, conditionKey)
	case permissionFlowLocalPermission:
		if outcome.Kind == permissionOutcomeActivateBoundaryScope {
			fields := append(flow.summaryLogFields(
				errclass.ClassActionable,
				conditionKey,
				outcome.TriggerPath,
				outcome.ScopeKey,
			),
				slog.String("boundary", outcome.BoundaryPath),
				slog.String("trigger_path", outcome.TriggerPath),
			)
			flow.engine.logger.Info("local permission denied: directory blocked", fields...)
		}
	default:
		panic(fmt.Sprintf("unknown permission flow %d", flowKind))
	}
}

func (flow *engineFlow) logRemotePermissionOutcome(
	outcome *PermissionOutcome,
	conditionKey ConditionKey,
) {
	if outcome == nil || !outcome.IsBoundaryFailure() {
		return
	}

	scopeKey := outcome.ScopeKey
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKey,
		outcome.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", outcome.BoundaryPath),
		slog.String("trigger_path", outcome.TriggerPath),
	)
	flow.engine.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (flow *engineFlow) recordRetryWorkFailure(
	ctx context.Context,
	kind PermissionOutcomeKind,
	failure *RetryWorkFailure,
) error {
	if failure == nil {
		return nil
	}

	conditionKey := ConditionKeyForStoredCondition(failure.ConditionType, failure.ScopeKey)
	logClass := errclass.ClassActionable
	if failure.Blocked {
		logClass = errclass.ClassBlockScopeingTransient
	}

	var (
		row RetryWorkRow
		err error
	)
	if failure.Blocked {
		conditionKey = ConditionKeyForStoredCondition(failure.ScopeKey.ConditionType(), failure.ScopeKey)
		row, err = flow.persistBlockedRetryWork(ctx, retryWorkKey(failure.Path, failure.OldPath, failure.ActionType), failure)
		if err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", failure.Path, err)
		}
	} else {
		delayFn := permissionOutcomeRetryDelay(kind, failure)
		persisted, persistErr := flow.engine.baseline.RecordRetryWorkFailure(ctx, failure, delayFn)
		if persistErr != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", failure.Path, persistErr)
		}
		if persisted == nil {
			return fmt.Errorf("record permission retry_work for %s: missing persisted row", failure.Path)
		}
		row = *persisted
		flow.retryRowsByKey[retryWorkKeyForRetryWork(persisted)] = row
	}

	fields := append(flow.summaryLogFields(
		logClass,
		conditionKey,
		failure.Path,
		failure.ScopeKey,
	),
		slog.String("condition_type", retryConditionTypeForRow(&row)),
	)
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
