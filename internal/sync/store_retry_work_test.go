package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "drop.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  20,
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
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "dest.txt",
		OldPath:      "src-b.txt",
		ActionType:   ActionRemoteMove,
		AttemptCount: 1,
	}))

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, []RetryWorkKey{
		{Path: "dest.txt", OldPath: "src-b.txt", ActionType: ActionRemoteMove},
	}))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src-b.txt", rows[0].OldPath)
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
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retry-later.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Hour).UnixNano(),
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
	}))

	blocked, err := store.ListBlockedRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Equal(t, "blocked.txt", blocked[0].Path)
	assert.Equal(t, SKService(), blocked[0].ScopeKey)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	readyCount := 0
	for i := range rows {
		if !rows[i].Blocked && rows[i].NextRetryAt > 0 && rows[i].NextRetryAt <= now.UnixNano() {
			readyCount++
			assert.Equal(t, "retry-now.txt", rows[i].Path)
		}
	}
	assert.Equal(t, 1, readyCount)
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
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "retrying-b.txt",
		ActionType:   ActionDownload,
		AttemptCount: 4,
		NextRetryAt:  20,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 7,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "fresh.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  30,
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
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked-a.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 2,
	}))
	require.NoError(t, store.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked-b.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 3,
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
func TestScanRetryWorkRow_NilDestination(t *testing.T) {
	t.Parallel()

	err := scanRetryWorkRow(nilRetryWorkScanner{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil destination")
}

type nilRetryWorkScanner struct{}

func (nilRetryWorkScanner) Scan(...any) error {
	return nil
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
		_, recordErr := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("", "", ActionUpload), func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("bad-action.txt", "", ActionType(-1)), func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid action type")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("needs-delay.txt", "", ActionUpload), nil)
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires delay function")

	err = func() error {
		_, recordErr := store.RecordBlockedRetryWork(ctx, testRetryWorkKey("blocked-without-scope.txt", "", ActionUpload), ScopeKey{})
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scope key")
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_PopulatesRetryAndBlockedRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(100, 0)
	setStoreTestNow(store, now)

	_, err := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("retry.txt", "", ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	_, err = store.RecordBlockedRetryWork(ctx, testRetryWorkKey("blocked.txt", "", ActionRemoteDelete), SKService())
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

	assert.True(t, byPath["blocked.txt"].Blocked)
	assert.Equal(t, int64(0), byPath["blocked.txt"].NextRetryAt)
	assert.Equal(t, SKService(), byPath["blocked.txt"].ScopeKey)
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_PreservesMoveOldPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("dest.txt", "src.txt", ActionRemoteMove), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "dest.txt", rows[0].Path)
	assert.Equal(t, "src.txt", rows[0].OldPath)
}

// Validates: R-2.10.33
func TestClearBlockedRetryWork_RemovesOnlyMatchingScopedWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordBlockedRetryWork(ctx, testRetryWorkKey("blocked.txt", "", ActionUpload), SKService())
	require.NoError(t, err)

	other := testRetryWorkRow("blocked.txt", "old.txt", ActionRemoteMove)
	other.ScopeKey = SKService()
	other.Blocked = true
	other.AttemptCount = 2
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

	_, err := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("docs/report.txt", "", ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	row, found, err := store.ResolveRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: ActionUpload,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.Equal(t, ActionUpload, row.ActionType)
	assert.Equal(t, 1, row.AttemptCount)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.10.33
func TestResolveRetryWork_PreservesUnrelatedRetryWorkOnSamePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("docs/report.txt", "", ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	other := testRetryWorkRow("docs/report.txt", "old.txt", ActionRemoteMove)
	other.AttemptCount = 2
	other.NextRetryAt = time.Now().Add(time.Minute).UnixNano()
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

func TestResolveRetryWork_DeletesRetryWorkWithoutIssueRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	row := testRetryWorkRow("docs/report.txt", "old.txt", ActionRemoteMove)
	row.AttemptCount = 3
	row.NextRetryAt = time.Now().Add(time.Minute).UnixNano()
	require.NoError(t, store.UpsertRetryWork(ctx, &row))

	resolved, found, err := store.ResolveRetryWork(ctx, RetryWorkKey{
		Path:       "docs/report.txt",
		OldPath:    "old.txt",
		ActionType: ActionRemoteMove,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, resolved)
	assert.Equal(t, "docs/report.txt", resolved.Path)
	assert.Equal(t, "old.txt", resolved.OldPath)
	assert.Equal(t, ActionRemoteMove, resolved.ActionType)
	assert.Equal(t, 3, resolved.AttemptCount)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
