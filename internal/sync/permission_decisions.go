package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/failures"
)

type PermissionCheckDecisionKind int

const (
	permissionCheckNone PermissionCheckDecisionKind = iota
	permissionCheckRecordFileFailure
	permissionCheckActivateBoundaryScope
	permissionCheckActivateDerivedScope
)

// PermissionCheckDecision is the policy-layer output for a single permission
// check performed during worker-result handling. Matched=false means the engine
// should fall back to generic failure recording.
type PermissionCheckDecision struct {
	Matched      bool
	Kind         PermissionCheckDecisionKind
	Failure      SyncFailureParams
	ScopeKey     ScopeKey
	ScopeBlock   ScopeBlock
	BoundaryPath string
	TriggerPath  string
}

type PermissionRecheckDecisionKind int

const (
	permissionRecheckKeepScope PermissionRecheckDecisionKind = iota
	permissionRecheckReleaseScope
	permissionRecheckClearFileFailure
)

// PermissionRecheckDecision is the policy-layer output for startup/per-pass
// permission maintenance. The engine applies these decisions through its owned
// failure and scope lifecycle methods.
type PermissionRecheckDecision struct {
	Kind     PermissionRecheckDecisionKind
	Path     string
	DriveID  driveid.ID
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
	case permissionCheckRecordFileFailure:
		controller.recordExplicitFailure(ctx, &decision.Failure)
	case permissionCheckActivateBoundaryScope:
		controller.recordExplicitFailure(ctx, &decision.Failure)
		if err := controller.activateScope(ctx, watch, &decision.ScopeBlock); err != nil {
			flow.engine.logger.Warn("failed to activate permission scope",
				slog.String("scope_key", decision.ScopeBlock.Key.String()),
				slog.String("error", err.Error()),
			)
		}
	case permissionCheckActivateDerivedScope:
		controller.recordExplicitFailure(ctx, &decision.Failure)
		if watch != nil {
			watch.upsertActiveScope(&ScopeBlock{
				Key:       decision.ScopeKey,
				IssueType: decision.ScopeKey.IssueType(),
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
	summaryKey := SummaryKeyForPersistedFailure(
		decision.Failure.IssueType,
		decision.Failure.Category,
		decision.Failure.Role,
	)

	switch flowKind {
	case permissionFlowNone:
		return
	case permissionFlowRemote403:
		controller.logRemotePermissionDecision(decision, summaryKey)
	case permissionFlowLocalPermission:
		if decision.Kind == permissionCheckActivateBoundaryScope {
			fields := append(controller.flow.summaryLogFields(
				failures.ClassActionable,
				summaryKey,
				decision.TriggerPath,
				decision.ScopeBlock.Key,
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

func (controller *scopeController) logRemotePermissionDecision(
	decision *PermissionCheckDecision,
	summaryKey SummaryKey,
) {
	if decision.Kind != permissionCheckActivateBoundaryScope && decision.Kind != permissionCheckActivateDerivedScope {
		return
	}

	scopeKey := decision.ScopeKey
	if scopeKey.IsZero() {
		scopeKey = decision.ScopeBlock.Key
	}
	fields := append(controller.flow.summaryLogFields(
		failures.ClassActionable,
		summaryKey,
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
		case permissionRecheckClearFileFailure:
			if err := flow.engine.baseline.ClearSyncFailure(ctx, decision.Path, decision.DriveID); err != nil {
				flow.engine.logger.Warn("failed to clear permission failure",
					slog.String("path", decision.Path),
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

func (controller *scopeController) recordExplicitFailure(ctx context.Context, params *SyncFailureParams) {
	flow := controller.flow
	summaryKey := SummaryKeyForPersistedFailure(params.IssueType, params.Category, params.Role)

	if err := flow.engine.baseline.RecordFailure(ctx, params, nil); err != nil {
		fields := append(flow.summaryLogFields(
			failures.ClassActionable,
			summaryKey,
			params.Path,
			params.ScopeKey,
		), slog.String("error", err.Error()))
		flow.engine.logger.Warn("failed to record permission failure", fields...)
		return
	}

	fields := append(flow.summaryLogFields(
		failures.ClassActionable,
		summaryKey,
		params.Path,
		params.ScopeKey,
	),
		slog.String("issue_type", params.IssueType),
	)
	flow.engine.logger.Debug("permission failure recorded", fields...)
}
