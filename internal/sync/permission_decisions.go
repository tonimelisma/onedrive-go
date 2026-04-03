package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type PermissionCheckDecisionKind int

const (
	permissionCheckNone PermissionCheckDecisionKind = iota
	permissionCheckRecordFileFailure
	permissionCheckActivateBoundaryScope
)

// PermissionCheckDecision is the policy-layer output for a single permission
// check performed during worker-result handling. Matched=false means the engine
// should fall back to generic failure recording.
type PermissionCheckDecision struct {
	Matched      bool
	Kind         PermissionCheckDecisionKind
	Failure      synctypes.SyncFailureParams
	ScopeBlock   synctypes.ScopeBlock
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
	ScopeKey synctypes.ScopeKey
	Reason   string
}

// ShortcutRemovalDecision explicitly describes scope state to discard when a
// shortcut disappears. The engine applies these through discardScope.
type ShortcutRemovalDecision struct {
	ScopeKey synctypes.ScopeKey
}

func (controller *scopeController) applyPermissionCheckDecision(
	ctx context.Context,
	watch *watchRuntime,
	flowKind permissionFlow,
	decision *PermissionCheckDecision,
) bool {
	flow := controller.flow

	if decision == nil || !decision.Matched {
		return false
	}

	switch decision.Kind {
	case permissionCheckNone:
	case permissionCheckRecordFileFailure:
		controller.recordExplicitFailure(ctx, &decision.Failure)
	case permissionCheckActivateBoundaryScope:
		controller.recordExplicitFailure(ctx, &decision.Failure)
		if err := controller.activateScope(ctx, watch, decision.ScopeBlock); err != nil {
			flow.engine.logger.Warn("failed to activate permission scope",
				slog.String("scope_key", decision.ScopeBlock.Key.String()),
				slog.String("error", err.Error()),
			)
		}
	default:
		panic(fmt.Sprintf("unknown permission check decision kind %d", decision.Kind))
	}

	switch flowKind {
	case permissionFlowNone:
	case permissionFlowRemote403:
		if decision.Kind == permissionCheckActivateBoundaryScope {
			flow.engine.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively",
				slog.String("boundary", decision.BoundaryPath),
				slog.String("trigger_path", decision.TriggerPath),
				slog.String("scope_key", decision.ScopeBlock.Key.String()),
			)
		}
	case permissionFlowLocalPermission:
		if decision.Kind == permissionCheckActivateBoundaryScope {
			flow.engine.logger.Info("local permission denied: directory blocked",
				slog.String("boundary", decision.BoundaryPath),
				slog.String("trigger_path", decision.TriggerPath),
			)
		}
	}

	return true
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

func (controller *scopeController) recordExplicitFailure(ctx context.Context, params *synctypes.SyncFailureParams) {
	flow := controller.flow

	if err := flow.engine.baseline.RecordFailure(ctx, params, nil); err != nil {
		flow.engine.logger.Warn("failed to record permission failure",
			slog.String("path", params.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (controller *scopeController) applyShortcutRemovalDecisions(ctx context.Context, decisions []ShortcutRemovalDecision) {
	flow := controller.flow

	for i := range decisions {
		if err := controller.discardScope(ctx, nil, decisions[i].ScopeKey); err != nil {
			flow.engine.logger.Warn("failed to discard shortcut scope",
				slog.String("scope_key", decisions[i].ScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
	}
}
