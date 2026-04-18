package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.11
func TestSyncStore_SetScopeRetryAtNow_UnblocksScopeFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Insert 2 scope-blocked (NULL next_retry_at) + 1 with next_retry_at set.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleHeld, Category: CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleHeld, Category: CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "c.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleItem, Category: CategoryTransient, ScopeKey: sk,
	}, func(int) time.Duration { return time.Hour })) // already has retry time

	// Unblock scope failures.
	affected, err := mgr.SetScopeRetryAtNow(ctx, sk, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), affected, "should update only NULL next_retry_at rows")

	// Verify the 2 rows now have next_retry_at = now.
	rows := readyRetryStateForTest(t, mgr, ctx, now)
	assert.Len(t, rows, 2, "both scope-blocked failures should now be retryable")
}

// Validates: R-2.10.11
func TestSyncStore_SetScopeRetryAtNow_NoMatches(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	affected, err := mgr.SetScopeRetryAtNow(ctx, SKService(), now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected)
}

// ---------------------------------------------------------------------------
// ReleaseScope / DiscardScope
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestSyncStore_ReleaseScope(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create a scope block.
	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))

	// Create the actionable boundary row plus held descendants.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "quota-boundary", DriveID: driveID, Direction: DirectionUpload,
		Role:     FailureRoleBoundary,
		Category: CategoryActionable, IssueType: IssueQuotaExceeded, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "x.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleHeld, Category: CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "y.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleHeld, Category: CategoryTransient, ScopeKey: sk,
	}, nil))

	err := mgr.ReleaseScope(ctx, sk, now)
	require.NoError(t, err)

	// Verify scope block is gone.
	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "scope block should be deleted")

	// Verify failures now have next_retry_at = now (retryable).
	rows := readyRetryStateForTest(t, mgr, ctx, now)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")

	allRows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, allRows, 2, "release should remove the actionable boundary row")
}

// Validates: R-2.10.11
func TestSyncStore_ReleaseScope_NoScopeBlock(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Should not error even if scope block doesn't exist.
	err := mgr.ReleaseScope(ctx, SKService(), now)
	require.NoError(t, err)
}

// Validates: R-2.10.11
func TestSyncStore_DiscardScope_RejectsZeroScopeKey(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)

	err := mgr.DiscardScope(context.Background(), ScopeKey{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scope key")
}

// Validates: R-2.10.38
func TestSyncStore_DiscardScope(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	scopeKey := SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          scopeKey,
		IssueType:    IssueQuotaExceeded,
		TimingSource: ScopeTimingNone,
		BlockedAt:    now.Add(-time.Minute),
	}))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/Docs", DriveID: driveID, Direction: DirectionUpload,
		Role:     FailureRoleBoundary,
		Category: CategoryActionable, IssueType: IssueQuotaExceeded, ScopeKey: scopeKey,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/Docs/a.txt", DriveID: driveID, Direction: DirectionUpload,
		Role: FailureRoleHeld, Category: CategoryTransient, ScopeKey: scopeKey,
	}, nil))

	err := mgr.DiscardScope(ctx, scopeKey)
	require.NoError(t, err)

	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "discard should delete the persisted scope row")

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "discard should delete all scoped failures")
}
