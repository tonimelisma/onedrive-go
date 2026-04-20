package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
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
	BlockScope       *ActiveScope
	BoundaryPath     string
	TriggerPath      string
}

type PermissionRecheckDecisionKind int

const (
	permissionRecheckKeepScope PermissionRecheckDecisionKind = iota
	permissionRecheckReleaseScope
)

// PermissionRecheckDecision is the policy-layer output for startup/per-pass
// permission maintenance. The engine applies these decisions through its owned
// block-scope lifecycle methods.
type PermissionRecheckDecision struct {
	Kind     PermissionRecheckDecisionKind
	Path     string
	ScopeKey ScopeKey
	Reason   string
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
	flow := controller.flow

	switch decision.Kind {
	case permissionCheckNone:
		return
	case permissionCheckRecordFileFailure,
		permissionCheckActivateBoundaryScope,
		permissionCheckActivateDerivedScope:
		controller.recordRetryWorkFailure(ctx, decision.RetryWorkFailure)
		if decision.BlockScope != nil {
			if err := controller.activateScope(ctx, watch, decision.BlockScope); err != nil {
				flow.engine.logger.Warn("failed to activate permission scope",
					slog.String("scope_key", decision.BlockScope.Key.String()),
					slog.String("error", err.Error()),
				)
			}
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
			scopeKey := decision.ScopeKey
			if scopeKey.IsZero() && decision.BlockScope != nil {
				scopeKey = decision.BlockScope.Key
			}

			fields := append(controller.flow.summaryLogFields(
				errclass.ClassActionable,
				conditionKey,
				decision.TriggerPath,
				scopeKey,
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

	if decision.BlockScope != nil {
		return ConditionKeyForStoredCondition(decision.BlockScope.Key.ConditionType(), decision.BlockScope.Key)
	}

	if decision.RetryWorkFailure != nil {
		return ConditionKeyForStoredCondition(
			decision.RetryWorkFailure.ConditionType,
			decision.RetryWorkFailure.ScopeKey,
		)
	}

	if !decision.ScopeKey.IsZero() {
		return ConditionKeyForStoredCondition("", decision.ScopeKey)
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
	if scopeKey.IsZero() && decision.BlockScope != nil {
		scopeKey = decision.BlockScope.Key
	}
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

func (controller *scopeController) applyPermissionRecheckDecisions(
	ctx context.Context,
	watch *watchRuntime,
	decisions []PermissionRecheckDecision,
) {
	flow := controller.flow

	for i := range decisions {
		decision := decisions[i]
		switch decision.Kind {
		case permissionRecheckKeepScope:
			continue
		case permissionRecheckReleaseScope:
			if err := controller.releaseScope(ctx, watch, decision.ScopeKey); err != nil {
				flow.engine.logger.Warn("failed to release permission scope",
					slog.String("scope_key", decision.ScopeKey.String()),
					slog.String("error", err.Error()),
				)
				continue
			}
		default:
			panic(fmt.Sprintf("unknown permission recheck decision kind %d", decision.Kind))
		}

		flow.engine.logger.Info(decision.Reason,
			slog.String("path", decision.Path),
			slog.String("scope_key", decision.ScopeKey.String()),
		)
	}
}

func (controller *scopeController) recordRetryWorkFailure(ctx context.Context, failure *RetryWorkFailure) {
	if failure == nil {
		return
	}

	flow := controller.flow
	conditionKey := ConditionKeyForStoredCondition(failure.ConditionType, failure.ScopeKey)

	if _, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, failure, nil); err != nil {
		fields := append(flow.summaryLogFields(
			errclass.ClassBlockScopeingTransient,
			conditionKey,
			failure.Path,
			failure.ScopeKey,
		), slog.String("error", err.Error()))
		flow.engine.logger.Warn("failed to record retry_work permission blocker", fields...)
		return
	}

	fields := append(flow.summaryLogFields(
		errclass.ClassBlockScopeingTransient,
		conditionKey,
		failure.Path,
		failure.ScopeKey,
	),
		slog.String("condition_type", failure.ConditionType),
	)
	flow.engine.logger.Debug("retry_work blocker recorded", fields...)
}
