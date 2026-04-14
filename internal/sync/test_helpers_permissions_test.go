package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	sharedTestFileItemID = "f1"
	sharedTestNFDResume  = "re\u0301sume\u0301.txt"
	sharedTestNFCResume  = "r\u00e9sum\u00e9.txt"
)

type mockPermChecker struct {
	perms map[string][]graph.Permission
	errs  map[string]error
}

func (m *mockPermChecker) ListItemPermissions(
	_ context.Context,
	driveID driveid.ID,
	itemID string,
) ([]graph.Permission, error) {
	key := driveID.String() + ":" + itemID
	if err := m.errs[key]; err != nil {
		return nil, err
	}

	return m.perms[key], nil
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

func requireSinglePermissionDecision(
	t *testing.T,
	decisions []PermissionRecheckDecision,
	wantKind PermissionRecheckDecisionKind,
) {
	t.Helper()

	require.Len(t, decisions, 1)
	require.Equal(t, wantKind, decisions[0].Kind)
}
