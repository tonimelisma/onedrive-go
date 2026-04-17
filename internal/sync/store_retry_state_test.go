package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3
func TestUpsertRetryStateAndPruneToCurrentActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "keep.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  10,
		LastError:    "keep me",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		PlanID:       "old-plan",
		Path:         "drop.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  20,
		LastError:    "drop me",
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))

	require.NoError(t, store.PruneRetryStateToCurrentActions(ctx, []RetryWorkKey{
		{Path: "keep.txt", ActionType: ActionUpload},
	}))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "keep.txt", rows[0].Path)
	assert.Equal(t, ActionUpload, rows[0].ActionType)
}

// Validates: R-2.10.33
func TestRetryStateReadyAndTrialCandidateQueries(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(50, 0)

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "retry-now.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  now.UnixNano(),
		LastError:    "retry",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "retry-later.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Hour).UnixNano(),
		LastError:    "later",
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
		LastError:    "blocked",
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	ready, err := store.ListRetryStateReady(ctx, now)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "retry-now.txt", ready[0].Path)

	candidate, found, err := store.PickRetryTrialCandidate(ctx, SKService())
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, candidate)
	assert.Equal(t, "blocked.txt", candidate.Path)
}

// Validates: R-2.10.33
func TestPruneScopeBlocksWithoutBlockedRetries(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(100, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(160, 0),
	}))
	require.NoError(t, store.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:           SKThrottleAccount(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(200, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(260, 0),
	}))

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "blocked.txt",
		ActionType:   ActionUpload,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
		LastError:    "blocked",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))

	require.NoError(t, store.PruneScopeBlocksWithoutBlockedRetries(ctx))

	blocks, err := store.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKService(), blocks[0].Key)
}

// Validates: R-2.10.33
func TestRecordFailure_MirrorsTransientAndHeldRowsIntoRetryState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	now := time.Unix(100, 0)
	store.SetNowFunc(func() time.Time { return now })

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "retry.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "retry me",
	}, func(int) time.Duration { return time.Minute }))

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "blocked.txt",
		DriveID:    driveID,
		Direction:  DirectionDelete,
		ActionType: ActionRemoteDelete,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   SKService(),
		IssueType:  IssueServiceOutage,
		ErrMsg:     "blocked",
	}, nil))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byPath := make(map[string]RetryStateRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}

	assert.False(t, byPath["retry.txt"].Blocked)
	assert.Equal(t, now.Add(time.Minute).UnixNano(), byPath["retry.txt"].NextRetryAt)
	assert.Empty(t, byPath["retry.txt"].PlanID)

	assert.True(t, byPath["blocked.txt"].Blocked)
	assert.Equal(t, int64(0), byPath["blocked.txt"].NextRetryAt)
	assert.Equal(t, SKService(), byPath["blocked.txt"].ScopeKey)
}

// Validates: R-2.10.33
func TestMarkSyncFailureActionableAndSetScopeRetryAtNow_UpdateRetryState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	now := time.Unix(200, 0)
	store.SetNowFunc(func() time.Time { return now })

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "actionable.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "retry me",
	}, func(int) time.Duration { return time.Minute }))
	require.NoError(t, store.MarkSyncFailureActionable(ctx, "actionable.txt", driveID))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "blocked.txt",
		DriveID:    driveID,
		Direction:  DirectionDelete,
		ActionType: ActionRemoteDelete,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   SKService(),
		IssueType:  IssueServiceOutage,
		ErrMsg:     "blocked",
	}, nil))
	affected, err := store.SetScopeRetryAtNow(ctx, SKService(), now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected)

	rows, err = store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Blocked)
	assert.Equal(t, now.Add(2*time.Minute).UnixNano(), rows[0].NextRetryAt)
}
