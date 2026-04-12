package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func applyRemote403Decision(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	bl *Baseline,
	path string,
	shortcuts []synctypes.Shortcut,
) PermissionCheckDecision {
	t.Helper()

	decision := eng.permHandler.handle403(ctx, bl, path, ActionUpload, shortcuts)
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		rt = newWatchRuntime(eng.Engine)
		rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	}
	rt.scopeController().applyPermissionCheckDecision(ctx, rt, permissionFlowRemote403, &decision)
	return decision
}

func applyLocalPermissionDecision(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	result *WorkerResult,
) PermissionCheckDecision {
	t.Helper()

	decision := eng.permHandler.handleLocalPermission(ctx, result)
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		rt = newWatchRuntime(eng.Engine)
		rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	}
	rt.scopeController().applyPermissionCheckDecision(ctx, rt, permissionFlowLocalPermission, &decision)
	return decision
}

func applyRemotePermissionRecheck(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	bl *Baseline,
	shortcuts []synctypes.Shortcut,
) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.recheckPermissions(ctx, bl, shortcuts)
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		rt = newWatchRuntime(eng.Engine)
		rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	}
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
	return decisions
}

func applyLocalPermissionRecheck(t *testing.T, eng *testEngine, ctx context.Context) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.recheckLocalPermissions(ctx)
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		rt = newWatchRuntime(eng.Engine)
		rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	}
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
	return decisions
}

func applyScannerResolvedPermissions(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	observed map[string]bool,
) []PermissionRecheckDecision {
	t.Helper()

	decisions := eng.permHandler.clearScannerResolvedPermissions(ctx, observed)
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		rt = newWatchRuntime(eng.Engine)
		rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	}
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
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
