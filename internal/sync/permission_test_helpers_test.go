package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func applyRemote403Decision(
	t *testing.T,
	eng *Engine,
	ctx context.Context,
	bl *synctypes.Baseline,
	path string,
	shortcuts []synctypes.Shortcut,
) PermissionCheckDecision {
	t.Helper()

	decision := eng.permHandler.handle403(ctx, bl, path, shortcuts)
	eng.applyPermissionCheckDecision(ctx, permissionFlowRemote403, &decision)
	return decision
}

func applyLocalPermissionDecision(
	t *testing.T,
	eng *Engine,
	ctx context.Context,
	result *synctypes.WorkerResult,
) PermissionCheckDecision {
	t.Helper()

	decision := eng.permHandler.handleLocalPermission(ctx, result)
	eng.applyPermissionCheckDecision(ctx, permissionFlowLocalPermission, &decision)
	return decision
}

func applyRemotePermissionRecheck(
	t *testing.T,
	eng *Engine,
	ctx context.Context,
	bl *synctypes.Baseline,
	shortcuts []synctypes.Shortcut,
) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.recheckPermissions(ctx, bl, shortcuts)
	eng.applyPermissionRecheckDecisions(ctx, decisions)
	return decisions
}

func applyLocalPermissionRecheck(t *testing.T, eng *Engine, ctx context.Context) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.recheckLocalPermissions(ctx)
	eng.applyPermissionRecheckDecisions(ctx, decisions)
	return decisions
}

func applyScannerResolvedPermissions(
	t *testing.T,
	eng *Engine,
	ctx context.Context,
	observed map[string]bool,
) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.clearScannerResolvedPermissions(ctx, observed)
	eng.applyPermissionRecheckDecisions(ctx, decisions)
	return decisions
}

func requireSinglePermissionDecision(
	t *testing.T,
	decisions []PermissionRecheckDecision,
	wantKind PermissionRecheckDecisionKind,
) {
	t.Helper()

	require.Len(t, decisions, 1)
	require.Equal(t, wantKind, decisions[0].Kind)
}
