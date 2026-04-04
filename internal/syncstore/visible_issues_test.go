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

// Validates: R-2.14.3, R-2.10.47
func TestSyncStore_ListVisibleIssueGroups(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)
	scopeKey := synctypes.SKPermRemote("Shared/Docs")

	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "bad:name.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueInvalidFilename,
		ErrMsg:     "invalid",
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs/a.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "blocked",
		ScopeKey:   scopeKey,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Docs/b.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "blocked",
		ScopeKey:   scopeKey,
	}, nil))
	require.NoError(t, mgr.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:          synctypes.SKAuthAccount(),
		IssueType:    synctypes.IssueUnauthorized,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    time.Unix(1, 0),
	}))
	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 4, 3, 10, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RequestScopeRecheck(ctx, scopeKey))

	target, found, err := mgr.FindRemoteBlockedTarget(ctx, "Shared/Docs/a.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, mgr.RequestRemoteBlockedTrial(ctx, target))

	groups, err := mgr.ListVisibleIssueGroups(ctx)
	require.NoError(t, err)

	require.Len(t, groups, 3)
	assert.Equal(t, synctypes.SummaryAuthenticationRequired, groups[0].SummaryKey)
	assert.Equal(t, synctypes.SummaryInvalidFilename, groups[1].SummaryKey)
	assert.Equal(t, synctypes.SummarySharedFolderWritesBlocked, groups[2].SummaryKey)
	assert.Equal(t, 2, groups[2].Count)
	assert.Equal(t, 1, groups[2].VisibleCount)
	require.NotNil(t, groups[2].RemoteBlocked)
	assert.ElementsMatch(t, []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"}, groups[2].RemoteBlocked.BlockedPaths)
	assert.True(t, groups[2].RemoteBlocked.HasManualTrial)
	assert.NotZero(t, groups[2].RemoteBlocked.RecheckRequestedAt)

	summary, err := mgr.ReadVisibleIssueSummary(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []IssueGroupCount{
		{Key: synctypes.SummaryAuthenticationRequired, Count: 1},
		{Key: synctypes.SummaryInvalidFilename, Count: 1},
		{Key: synctypes.SummarySharedFolderWritesBlocked, Count: 1},
	}, summary.Groups)
}

// Validates: R-2.14.5
func TestSyncStore_RequestScopeRecheck(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	scopeKey := synctypes.SKPermRemote("Shared/Docs")

	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 4, 3, 10, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RequestScopeRecheck(ctx, scopeKey))

	keys, err := mgr.ListRequestedScopeRechecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []synctypes.ScopeKey{scopeKey}, keys)

	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 4, 3, 11, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RequestScopeRecheck(ctx, scopeKey))

	keys, err = mgr.ListRequestedScopeRechecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []synctypes.ScopeKey{scopeKey}, keys)

	require.NoError(t, mgr.ClearScopeRecheckRequest(ctx, scopeKey))

	keys, err = mgr.ListRequestedScopeRechecks(ctx)
	require.NoError(t, err)
	assert.Empty(t, keys)
}
