package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.14.3, R-2.10.47
func TestSyncStore_ListVisibleIssueGroups(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "bad:name.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryActionable,
		IssueType:  IssueInvalidFilename,
		ErrMsg:     "invalid",
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/Docs/a.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueRemoteWriteDenied,
		ErrMsg:     "blocked",
		ScopeKey:   scopeKey,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/Docs/b.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueRemoteWriteDenied,
		ErrMsg:     "blocked",
		ScopeKey:   scopeKey,
	}, nil))
	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          SKAuthAccount(),
		IssueType:    IssueUnauthorized,
		TimingSource: ScopeTimingNone,
		BlockedAt:    time.Unix(1, 0),
	}))

	groups, err := mgr.ListVisibleIssueGroups(ctx)
	require.NoError(t, err)

	require.Len(t, groups, 3)
	assert.Equal(t, SummaryAuthenticationRequired, groups[0].SummaryKey)
	assert.Equal(t, SummaryInvalidFilename, groups[1].SummaryKey)
	assert.Equal(t, SummaryRemoteWriteDenied, groups[2].SummaryKey)
	assert.Equal(t, 2, groups[2].Count)
	assert.Equal(t, 1, groups[2].VisibleCount)
	require.NotNil(t, groups[2].RemoteBlocked)
	assert.Equal(t, "Shared/Docs", groups[2].RemoteBlocked.BoundaryPath)
	assert.ElementsMatch(t, []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"}, groups[2].RemoteBlocked.BlockedPaths)

	summary, err := mgr.ReadVisibleIssueSummary(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []IssueGroupCount{
		{Key: SummaryAuthenticationRequired, Count: 1},
		{Key: SummaryInvalidFilename, Count: 1},
		{Key: SummaryRemoteWriteDenied, Count: 1},
	}, summary.Groups)

	count, err := mgr.CountVisibleIssues(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}
