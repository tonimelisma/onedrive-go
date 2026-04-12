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

// ---------------------------------------------------------------------------
// PickTrialCandidate
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestSyncStore_PickTrialCandidate_ReturnsOldestScopeBlockedFailure(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := synctypes.SKQuotaOwn()

	// Insert two scope-blocked failures (next_retry_at = NULL, scope_key matches).
	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "b.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, nil)) // nil delayFn → next_retry_at = NULL

	mgr.SetNowFunc(func() time.Time { return time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC) })
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "a.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, nil))

	// PickTrialCandidate should return a.txt (earliest first_seen_at).
	row, found, err := mgr.PickTrialCandidate(ctx, sk)
	require.NoError(t, err)
	require.True(t, found, "should find a scope-blocked failure")
	require.NotNil(t, row, "should find a scope-blocked failure")
	assert.Equal(t, "a.txt", row.Path)
	assert.Equal(t, sk, row.ScopeKey)
}

// Validates: R-2.10.5
func TestSyncStore_PickTrialCandidate_SkipsRetriedFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := synctypes.SKQuotaOwn()

	// Insert a failure WITH next_retry_at set (already being retried).
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "retried.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleItem,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, func(int) time.Duration { return time.Minute })) // sets next_retry_at

	// PickTrialCandidate should return nil — no NULL next_retry_at rows.
	row, found, err := mgr.PickTrialCandidate(ctx, sk)
	require.NoError(t, err)
	assert.False(t, found, "should not return failures with next_retry_at set")
	assert.Nil(t, row, "should not return failures with next_retry_at set")
}

// Validates: R-2.10.5
func TestSyncStore_PickTrialCandidate_NoMatches(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	// Empty table → nil, nil.
	row, found, err := mgr.PickTrialCandidate(ctx, synctypes.SKQuotaOwn())
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, row)
}

// ---------------------------------------------------------------------------
// SetScopeRetryAtNow
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestSyncStore_SetScopeRetryAtNow_UnblocksScopeFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := synctypes.SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Insert 2 scope-blocked (NULL next_retry_at) + 1 with next_retry_at set.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleHeld, Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleHeld, Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "c.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleItem, Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, func(int) time.Duration { return time.Hour })) // already has retry time

	// Unblock scope failures.
	affected, err := mgr.SetScopeRetryAtNow(ctx, sk, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), affected, "should update only NULL next_retry_at rows")

	// Verify the 2 rows now have next_retry_at = now.
	rows, err := mgr.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "both scope-blocked failures should now be retryable")
}

// Validates: R-2.10.11
func TestSyncStore_SetScopeRetryAtNow_NoMatches(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	affected, err := mgr.SetScopeRetryAtNow(ctx, synctypes.SKService(), now)
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
	sk := synctypes.SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create a scope block.
	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))

	// Create the actionable boundary row plus held descendants.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "quota-boundary", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleBoundary,
		Category: synctypes.CategoryActionable, IssueType: synctypes.IssueQuotaExceeded, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "x.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleHeld, Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "y.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleHeld, Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	err := mgr.ReleaseScope(ctx, sk, now)
	require.NoError(t, err)

	// Verify scope block is gone.
	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "scope block should be deleted")

	// Verify failures now have next_retry_at = now (retryable).
	rows, err := mgr.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
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
	err := mgr.ReleaseScope(ctx, synctypes.SKService(), now)
	require.NoError(t, err)
}

// Validates: R-2.10.38
func TestSyncStore_DiscardScope(t *testing.T) {
	t.Parallel()
	mgr := newTestStore(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	scopeKey := synctypes.SKQuotaOwn()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          scopeKey,
		IssueType:    synctypes.IssueQuotaExceeded,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    now.Add(-time.Minute),
	}))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/Docs", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleBoundary,
		Category: synctypes.CategoryActionable, IssueType: synctypes.IssueQuotaExceeded, ScopeKey: scopeKey,
	}, nil))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/Docs/a.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role: synctypes.FailureRoleHeld, Category: synctypes.CategoryTransient, ScopeKey: scopeKey,
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
