package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3
func TestUpsertRetryWorkAndPruneToCurrentActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "keep.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  10,
		LastError:    "keep me",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "drop.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  20,
		LastError:    "drop me",
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, []RetryWorkKey{
		{Path: "keep.txt", ActionType: ActionUpload},
	}))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "keep.txt", rows[0].Path)
	assert.Equal(t, ActionUpload, rows[0].ActionType)
}

// Validates: R-2.10.33
func TestRetryWorkPruneDistinguishesOldPathSemanticWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "dest.txt",
		OldPath:      "src-a.txt",
		ActionType:   ActionRemoteMove,
		AttemptCount: 1,
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "dest.txt",
		OldPath:      "src-b.txt",
		ActionType:   ActionRemoteMove,
		AttemptCount: 1,
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, []RetryWorkKey{
		{Path: "dest.txt", OldPath: "src-b.txt", ActionType: ActionRemoteMove},
	}))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src-b.txt", rows[0].OldPath)
	assert.NotEmpty(t, rows[0].WorkKey)
}

// Validates: R-2.10.33
func TestRetryWorkReadyAndTrialCandidateQueries(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(50, 0)

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retry-now.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  now.UnixNano(),
		LastError:    "retry",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retry-later.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Hour).UnixNano(),
		LastError:    "later",
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
		LastError:    "blocked",
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	ready, err := store.ListRetryWorkReady(ctx, now)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "retry-now.txt", ready[0].Path)

	candidate, found, err := store.PickRetryTrialCandidate(ctx, SKService())
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, candidate)
	assert.Equal(t, "blocked.txt", candidate.Path)

	blocked, err := store.ListBlockedRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Equal(t, "blocked.txt", blocked[0].Path)
	assert.Equal(t, SKService(), blocked[0].ScopeKey)
}

// Validates: R-2.10.33
func TestRetryWorkEarliestRetryAt_IgnoresBlockedRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(50, 0)

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionDownload,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
		NextRetryAt:  now.Add(10 * time.Minute).UnixNano(),
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "later.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(20 * time.Minute).UnixNano(),
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "earliest.txt",
		ActionType:   ActionLocalDelete,
		AttemptCount: 1,
		NextRetryAt:  now.Add(5 * time.Minute).UnixNano(),
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	earliest, err := store.EarliestRetryWorkAt(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(5*time.Minute), earliest)
}

// Validates: R-2.10.33
func TestCountRetryingWork_IgnoresBlockedAndLowAttemptRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retrying-a.txt",
		ActionType:   ActionUpload,
		AttemptCount: 3,
		NextRetryAt:  10,
		LastError:    "retrying",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retrying-b.txt",
		ActionType:   ActionDownload,
		AttemptCount: 4,
		NextRetryAt:  20,
		LastError:    "retrying",
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 7,
		LastError:    "blocked",
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "fresh.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  30,
		LastError:    "fresh",
		FirstSeenAt:  7,
		LastSeenAt:   8,
	}))

	count, err := store.CountRetryingWork(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// Validates: R-2.10.33
func TestRetryWorkScopeReadyAndDeleteHelpers(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(75, 0).UnixNano()

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "delete-me.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked-a.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked-b.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 3,
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	require.NoError(t, deleteRetryWorkByWorkTx(ctx, store.db, RetryWorkKey{
		Path:       "delete-me.txt",
		ActionType: ActionUpload,
	}))
	require.NoError(t, markRetryWorkScopeReadyTx(ctx, store.db, SKService().String(), now))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, SKService(), row.ScopeKey)
		assert.False(t, row.Blocked)
		assert.Equal(t, now, row.NextRetryAt)
	}

	require.NoError(t, deleteRetryWorkByScopeTx(ctx, store.db, SKService().String()))

	rows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.10.33
func TestRetryWorkPickTrialCandidate_NoRowsAndNilDestination(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	candidate, found, err := store.PickRetryTrialCandidate(ctx, SKService())
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, candidate)

	err = scanRetryWorkRow(nilRetryWorkScanner{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil destination")
}

type nilRetryWorkScanner struct{}

func (nilRetryWorkScanner) Scan(...any) error {
	return nil
}

// Validates: R-2.10.33
func TestPruneBlockScopesWithoutBlockedWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(100, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(160, 0),
	}))
	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKThrottleDrive(driveid.New("0000000000000001")),
		IssueType:     IssueRateLimited,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(200, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(260, 0),
	}))

	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionUpload,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
		LastError:    "blocked",
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))

	require.NoError(t, store.PruneBlockScopesWithoutBlockedWork(ctx))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKService(), blocks[0].Key)
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	err := func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, nil, nil)
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil failure")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			ActionType: ActionUpload,
			IssueType:  IssueServiceOutage,
		}, func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			Path:       "bad-action.txt",
			ActionType: ActionType(-1),
			IssueType:  IssueServiceOutage,
		}, func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid action type")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			Path:       "needs-delay.txt",
			ActionType: ActionUpload,
			IssueType:  IssueServiceOutage,
		}, nil)
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires delay function")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			Path:       "blocked-without-scope.txt",
			ActionType: ActionUpload,
			IssueType:  IssueRemoteWriteDenied,
			Blocked:    true,
		}, nil)
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires scope key")
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_PopulatesRetryAndBlockedRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(100, 0)
	setStoreTestNow(store, now)

	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "retry.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		LastError:  "retry me",
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	_, err = store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "blocked.txt",
		ActionType: ActionRemoteDelete,
		IssueType:  IssueServiceOutage,
		ScopeKey:   SKService(),
		LastError:  "blocked",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byPath := make(map[string]RetryWorkRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}

	assert.False(t, byPath["retry.txt"].Blocked)
	assert.Equal(t, now.Add(time.Minute).UnixNano(), byPath["retry.txt"].NextRetryAt)
	assert.NotEmpty(t, byPath["retry.txt"].WorkKey)
	assert.Equal(t, IssueServiceOutage, byPath["retry.txt"].IssueType)

	assert.True(t, byPath["blocked.txt"].Blocked)
	assert.Equal(t, int64(0), byPath["blocked.txt"].NextRetryAt)
	assert.Equal(t, SKService(), byPath["blocked.txt"].ScopeKey)
	assert.Equal(t, IssueServiceOutage, byPath["blocked.txt"].IssueType)
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_PreservesMoveOldPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "dest.txt",
		OldPath:    "src.txt",
		ActionType: ActionRemoteMove,
		IssueType:  IssueServiceOutage,
		LastError:  "move later",
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src.txt", rows[0].OldPath)
	assert.NotEmpty(t, rows[0].WorkKey)
}

// Validates: R-2.10.33
func TestClearBlockedRetryWork_RemovesOnlyMatchingScopedWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		ScopeKey:   SKService(),
		LastError:  "blocked upload",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	other := retryWorkIdentityForWork("blocked.txt", "old.txt", ActionRemoteMove)
	other.ScopeKey = SKService()
	other.Blocked = true
	other.AttemptCount = 2
	other.FirstSeenAt = time.Now().UnixNano()
	other.LastSeenAt = other.FirstSeenAt
	require.NoError(t, store.UpsertRetryWork(ctx, &other))

	require.NoError(t, store.ClearBlockedRetryWork(ctx, RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, SKService()))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "blocked.txt", rows[0].Path)
	assert.Equal(t, ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

// Validates: R-2.10.33
func TestResolveRetryWork_ReturnsAndDeletesMatchingWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		LastError:  "server error",
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	row, found, err := store.ResolveRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	}, row.Work)
	assert.Equal(t, 1, row.AttemptCount)
	assert.Equal(t, IssueServiceOutage, row.IssueType)
	assert.True(t, row.HadIssueRow)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.10.33
func TestResolveRetryWork_PreservesUnrelatedRetryWorkOnSamePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		LastError:  "server error",
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	other := retryWorkIdentityForWork("docs/report.txt", "old.txt", ActionRemoteMove)
	other.AttemptCount = 2
	other.NextRetryAt = time.Now().Add(time.Minute).UnixNano()
	other.FirstSeenAt = time.Now().UnixNano()
	other.LastSeenAt = other.FirstSeenAt
	require.NoError(t, store.UpsertRetryWork(ctx, &other))

	_, found, err := store.ResolveRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	})
	require.NoError(t, err)
	require.True(t, found)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

func TestResolveTransientRetryWork_DeletesRetryWorkWithoutIssueRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	row := retryWorkIdentityForWork("docs/report.txt", "old.txt", ActionRemoteMove)
	row.AttemptCount = 3
	row.NextRetryAt = time.Now().Add(time.Minute).UnixNano()
	row.FirstSeenAt = time.Now().UnixNano()
	row.LastSeenAt = row.FirstSeenAt
	require.NoError(t, store.UpsertRetryWork(ctx, &row))

	resolved, found, err := store.ResolveTransientRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		OldPath:    "old.txt",
		ActionType: ActionRemoteMove,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, resolved)
	assert.Equal(t, RetryWorkKey{
		Path:       "docs/report.txt",
		OldPath:    "old.txt",
		ActionType: ActionRemoteMove,
	}, resolved.Work)
	assert.Equal(t, 3, resolved.AttemptCount)
	assert.Empty(t, resolved.IssueType)
	assert.False(t, resolved.HadIssueRow)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
