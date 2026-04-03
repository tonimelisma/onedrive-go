package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	testRemoteBlockedBoundaryTeamDocs = "Shared/TeamDocs"
	testRemoteBlockedBoundaryFinance  = "Shared/Finance"
)

func recordRemoteBlockedFailure(
	t *testing.T,
	mgr *SyncStore,
	ctx context.Context,
	driveID driveid.ID,
	path string,
	boundary string,
	actionType synctypes.ActionType,
) {
	t.Helper()

	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       path,
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: actionType,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		ErrMsg:     "shared folder is read-only",
		ScopeKey:   synctypes.SKPermRemote(boundary),
	}, nil))
}

// Validates: R-2.14.3
func TestSyncStore_FindRemoteBlockedTarget_PrefersBoundaryMatch(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")
	boundary := testRemoteBlockedBoundaryTeamDocs
	childPath := boundary + "/draft.txt"

	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundary, boundary, synctypes.ActionFolderCreate)
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, childPath, boundary, synctypes.ActionUpload)

	target, found, err := mgr.FindRemoteBlockedTarget(ctx, boundary)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RemoteBlockedTargetBoundary, target.Kind)
	assert.Equal(t, synctypes.SKPermRemote(boundary), target.ScopeKey)

	target, found, err = mgr.FindRemoteBlockedTarget(ctx, childPath)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RemoteBlockedTargetPath, target.Kind)
	assert.Equal(t, childPath, target.Path)
	assert.Equal(t, driveID, target.DriveID)
	assert.Equal(t, synctypes.SKPermRemote(boundary), target.ScopeKey)

	_, found, err = mgr.FindRemoteBlockedTarget(ctx, "Shared/Missing")
	require.NoError(t, err)
	assert.False(t, found)
}

// Validates: R-2.14.3
func TestSyncStore_ClearRemoteBlockedTargets(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")
	boundaryA := testRemoteBlockedBoundaryTeamDocs
	boundaryB := testRemoteBlockedBoundaryFinance

	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundaryA+"/a.txt", boundaryA, synctypes.ActionUpload)
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundaryA+"/b.txt", boundaryA, synctypes.ActionUpload)
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundaryB+"/budget.xlsx", boundaryB, synctypes.ActionUpload)

	targetA, found, err := mgr.FindRemoteBlockedTarget(ctx, boundaryA+"/a.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, mgr.ClearRemoteBlockedTarget(ctx, targetA))

	rows, err := mgr.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{boundaryA + "/b.txt", boundaryB + "/budget.xlsx"}, []string{rows[0].Path, rows[1].Path})

	targetB, found, err := mgr.FindRemoteBlockedTarget(ctx, boundaryB)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, mgr.ClearRemoteBlockedTarget(ctx, targetB))

	rows, err = mgr.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, boundaryA+"/b.txt", rows[0].Path)

	require.NoError(t, mgr.ClearAllRemoteBlockedFailures(ctx))

	rows, err = mgr.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.14.5
func TestSyncStore_RequestRemoteBlockedTrial_ByPathOnly(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")
	boundaryA := testRemoteBlockedBoundaryTeamDocs
	boundaryB := testRemoteBlockedBoundaryFinance
	pathA := boundaryA + "/draft.txt"
	pathB := boundaryB + "/budget.xlsx"

	recordRemoteBlockedFailure(t, mgr, ctx, driveID, pathA, boundaryA, synctypes.ActionUpload)
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, pathB, boundaryB, synctypes.ActionUpload)

	boundaryTarget, found, err := mgr.FindRemoteBlockedTarget(ctx, boundaryA)
	require.NoError(t, err)
	require.True(t, found)
	require.ErrorIs(t, mgr.RequestRemoteBlockedTrial(ctx, boundaryTarget), ErrRemoteBlockedBoundaryRetry)

	targetA, found, err := mgr.FindRemoteBlockedTarget(ctx, pathA)
	require.NoError(t, err)
	require.True(t, found)
	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 4, 2, 10, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RequestRemoteBlockedTrial(ctx, targetA))

	targetB, found, err := mgr.FindRemoteBlockedTarget(ctx, pathB)
	require.NoError(t, err)
	require.True(t, found)
	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 4, 2, 11, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RequestRemoteBlockedTrial(ctx, targetB))

	keys, err := mgr.ListManualTrialScopeKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []synctypes.ScopeKey{
		synctypes.SKPermRemote(boundaryA),
		synctypes.SKPermRemote(boundaryB),
	}, keys)

	require.NoError(t, mgr.ClearManualTrialRequest(ctx, pathA, driveID))

	keys, err = mgr.ListManualTrialScopeKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []synctypes.ScopeKey{synctypes.SKPermRemote(boundaryB)}, keys)
}

func TestSyncStore_DropLegacyRemoteBlockedScope_RemovesOnlyLegacyAuthorityRows(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")
	boundary := testRemoteBlockedBoundaryTeamDocs
	scopeKey := synctypes.SKPermRemote(boundary)

	require.NoError(t, mgr.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:          scopeKey,
		IssueType:    synctypes.IssueSharedFolderBlocked,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    time.Date(2025, 4, 2, 10, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       boundary,
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		Role:       synctypes.FailureRoleBoundary,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssuePermissionDenied,
		ErrMsg:     "legacy boundary",
		ScopeKey:   scopeKey,
		ActionType: synctypes.ActionFolderCreate,
	}, nil))
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundary+"/draft.txt", boundary, synctypes.ActionUpload)

	require.NoError(t, mgr.DropLegacyRemoteBlockedScope(ctx, scopeKey))

	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, synctypes.FailureRoleHeld, rows[0].Role)
	assert.Equal(t, scopeKey, rows[0].ScopeKey)
	assert.Equal(t, boundary+"/draft.txt", rows[0].Path)
}
