package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// PickTrialCandidate
// ---------------------------------------------------------------------------

func TestSyncStore_PickTrialCandidate_ReturnsOldestScopeBlockedFailure(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn

	// Insert two scope-blocked failures (next_retry_at = NULL, scope_key matches).
	mgr.nowFunc = func() time.Time { return time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "b.txt",
		DriveID:   driveID,
		Direction: strUpload,
		Category:  strTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, nil)) // nil delayFn → next_retry_at = NULL

	mgr.nowFunc = func() time.Time { return time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC) }
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "a.txt",
		DriveID:   driveID,
		Direction: strUpload,
		Category:  strTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, nil))

	// PickTrialCandidate should return a.txt (earliest first_seen_at).
	row, err := mgr.PickTrialCandidate(ctx, sk)
	require.NoError(t, err)
	require.NotNil(t, row, "should find a scope-blocked failure")
	assert.Equal(t, "a.txt", row.Path)
	assert.Equal(t, sk, row.ScopeKey)
}

func TestSyncStore_PickTrialCandidate_SkipsRetriedFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn

	// Insert a failure WITH next_retry_at set (already being retried).
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:      "retried.txt",
		DriveID:   driveID,
		Direction: strUpload,
		Category:  strTransient,
		ErrMsg:    "quota exceeded",
		ScopeKey:  sk,
	}, func(int) time.Duration { return time.Minute })) // sets next_retry_at

	// PickTrialCandidate should return nil — no NULL next_retry_at rows.
	row, err := mgr.PickTrialCandidate(ctx, sk)
	require.NoError(t, err)
	assert.Nil(t, row, "should not return failures with next_retry_at set")
}

func TestSyncStore_PickTrialCandidate_NoMatches(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	// Empty table → nil, nil.
	row, err := mgr.PickTrialCandidate(ctx, SKQuotaOwn)
	require.NoError(t, err)
	assert.Nil(t, row)
}

// ---------------------------------------------------------------------------
// SetScopeRetryAtNow
// ---------------------------------------------------------------------------

func TestSyncStore_SetScopeRetryAtNow_UnblocksScopeFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Insert 2 scope-blocked (NULL next_retry_at) + 1 with next_retry_at set.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "c.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
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

func TestSyncStore_SetScopeRetryAtNow_NoMatches(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	affected, err := mgr.SetScopeRetryAtNow(ctx, SKService, now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected)
}

// ---------------------------------------------------------------------------
// ClearScopeAndUnblockFailures
// ---------------------------------------------------------------------------

func TestSyncStore_ClearScopeAndUnblockFailures(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create a scope block.
	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:       sk,
		IssueType: IssueQuotaExceeded,
		BlockedAt: now.Add(-time.Minute),
	}))

	// Create scope-blocked failures.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "x.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path: "y.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))

	// Clear scope and unblock failures atomically.
	err := mgr.ClearScopeAndUnblockFailures(ctx, sk, now)
	require.NoError(t, err)

	// Verify scope block is gone.
	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks, "scope block should be deleted")

	// Verify failures now have next_retry_at = now (retryable).
	rows, err := mgr.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")
}

func TestSyncStore_ClearScopeAndUnblockFailures_NoScopeBlock(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	ctx := context.Background()

	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	// Should not error even if scope block doesn't exist.
	err := mgr.ClearScopeAndUnblockFailures(ctx, SKService, now)
	require.NoError(t, err)
}
