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
func TestRetryStatePruneDistinguishesOldPathSemanticWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "dest.txt",
		OldPath:      "src-a.txt",
		ActionType:   ActionRemoteMove,
		AttemptCount: 1,
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "dest.txt",
		OldPath:      "src-b.txt",
		ActionType:   ActionRemoteMove,
		AttemptCount: 1,
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))

	require.NoError(t, store.PruneRetryStateToCurrentActions(ctx, []RetryWorkKey{
		{Path: "dest.txt", OldPath: "src-b.txt", ActionType: ActionRemoteMove},
	}))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src-b.txt", rows[0].OldPath)
	assert.NotEmpty(t, rows[0].WorkKey)
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

	blocked, err := store.ListBlockedRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Equal(t, "blocked.txt", blocked[0].Path)
	assert.Equal(t, SKService(), blocked[0].ScopeKey)
}

// Validates: R-2.10.33
func TestRetryStateEarliestRetryAt_IgnoresBlockedRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(50, 0)

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "blocked.txt",
		ActionType:   ActionDownload,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
		NextRetryAt:  now.Add(10 * time.Minute).UnixNano(),
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "later.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(20 * time.Minute).UnixNano(),
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "earliest.txt",
		ActionType:   ActionLocalDelete,
		AttemptCount: 1,
		NextRetryAt:  now.Add(5 * time.Minute).UnixNano(),
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	earliest, err := store.EarliestRetryStateAt(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(5*time.Minute), earliest)
}

// Validates: R-2.10.33
func TestRetryStateScopeReadyAndDeleteHelpers(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(75, 0).UnixNano()

	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "delete-me.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "blocked-a.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryState(ctx, &RetryStateRow{
		Path:         "blocked-b.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 3,
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	require.NoError(t, deleteRetryStateByWorkTx(ctx, store.db, RetryWorkKey{
		Path:       "delete-me.txt",
		ActionType: ActionUpload,
	}))
	require.NoError(t, markRetryStateScopeReadyTx(ctx, store.db, SKService().String(), now))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, SKService(), row.ScopeKey)
		assert.False(t, row.Blocked)
		assert.Equal(t, now, row.NextRetryAt)
	}

	require.NoError(t, deleteRetryStateByScopeTx(ctx, store.db, SKService().String()))

	rows, err = store.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.10.33
func TestRetryStatePickTrialCandidate_NoRowsAndNilDestination(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	candidate, found, err := store.PickRetryTrialCandidate(ctx, SKService())
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, candidate)

	err = scanRetryStateRow(nilRetryStateScanner{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil destination")
}

type nilRetryStateScanner struct{}

func (nilRetryStateScanner) Scan(...any) error {
	return nil
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
	assert.NotEmpty(t, byPath["retry.txt"].WorkKey)

	assert.True(t, byPath["blocked.txt"].Blocked)
	assert.Equal(t, int64(0), byPath["blocked.txt"].NextRetryAt)
	assert.Equal(t, SKService(), byPath["blocked.txt"].ScopeKey)
}

// Validates: R-2.10.33
func TestRecordFailure_MirrorsMoveOldPathIntoRetryState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "dest.txt",
		OldPath:    "src.txt",
		DriveID:    driveID,
		Direction:  DirectionDownload,
		ActionType: ActionRemoteMove,
		Role:       FailureRoleItem,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "move later",
	}, func(int) time.Duration { return time.Minute }))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src.txt", rows[0].OldPath)
	assert.NotEmpty(t, rows[0].WorkKey)
}

// Validates: R-2.10.33
func TestUpsertActionableFailureAndSetScopeRetryAtNow_UpdateRetryState(t *testing.T) {
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
	require.NoError(t, store.UpsertActionableFailures(ctx, []ActionableFailure{{
		Path:       "actionable.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		IssueType:  IssueInvalidFilename,
		Error:      "needs rename",
	}}))

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

// Validates: R-2.10.33
func TestClearHeldRetryWork_RemovesScopedFailureAndRetryRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "blocked.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   SKService(),
		IssueType:  IssueServiceOutage,
		ErrMsg:     "blocked",
	}, nil))

	require.NoError(t, store.ClearHeldRetryWork(ctx, RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, SKService()))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)

	failures, err := store.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

// Validates: R-2.10.33
func TestClearHeldRetryWork_PreservesOtherRetryStateOnSamePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "blocked.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   SKService(),
		IssueType:  IssueServiceOutage,
		ErrMsg:     "blocked upload",
	}, nil))

	other := retryStateIdentityForWork("blocked.txt", "old.txt", ActionRemoteMove)
	other.ScopeKey = SKService()
	other.Blocked = true
	other.AttemptCount = 2
	other.FirstSeenAt = time.Now().UnixNano()
	other.LastSeenAt = other.FirstSeenAt
	require.NoError(t, store.UpsertRetryState(ctx, &other))

	require.NoError(t, store.ClearHeldRetryWork(ctx, RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, SKService()))

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "blocked.txt", rows[0].Path)
	assert.Equal(t, ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

func TestResolveTransientRetryWork_ReturnsAndDeletesMatchingWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "docs/report.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error",
	}, func(int) time.Duration { return time.Minute }))

	row, found, err := store.ResolveTransientRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	}, driveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.Equal(t, ActionUpload, row.ActionType)

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)

	failures, err := store.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestResolveTransientRetryWork_PreservesUnrelatedRetryStateOnSamePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.RecordFailure(ctx, &SyncFailureParams{
		Path:       "docs/report.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error",
	}, func(int) time.Duration { return time.Minute }))

	other := retryStateIdentityForWork("docs/report.txt", "old.txt", ActionRemoteMove)
	other.AttemptCount = 2
	other.NextRetryAt = time.Now().Add(time.Minute).UnixNano()
	other.FirstSeenAt = time.Now().UnixNano()
	other.LastSeenAt = other.FirstSeenAt
	require.NoError(t, store.UpsertRetryState(ctx, &other))

	_, found, err := store.ResolveTransientRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	}, driveID)
	require.NoError(t, err)
	require.True(t, found)

	rows, err := store.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}
