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

	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("keep.txt", "", ActionUpload, 2, 10)))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("drop.txt", "", ActionDownload, 1, 20)))

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

	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("dest.txt", "src-a.txt", ActionRemoteMove, 1, 0)))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("dest.txt", "src-b.txt", ActionRemoteMove, 1, 0)))

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

	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("retry-now.txt", "", ActionDownload, 1, now.UnixNano())))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("retry-later.txt", "", ActionUpload, 1, now.Add(time.Hour).UnixNano())))
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked.txt", "", ActionRemoteDelete, SKService(), 2)))

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
func TestReadDriveStatusSnapshot_RetryingIgnoresBlockedAndLowAttemptRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("retrying-a.txt", "", ActionUpload, 3, 10)))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("retrying-b.txt", "", ActionDownload, 4, 20)))
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked.txt", "", ActionRemoteDelete, SKService(), 7)))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("fresh.txt", "", ActionUpload, 2, 30)))

	snapshot, err := store.ReadDriveStatusSnapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, snapshot.RetryingItems)
	assert.Len(t, snapshot.RetryingWork, snapshot.RetryingItems, "the count is the rows status lists")
}

// Validates: R-2.10.33
func TestRetryWorkScopeReadyAndDeleteHelpers(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Unix(75, 0).UnixNano()

	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("delete-me.txt", "", ActionUpload, 1, 0)))
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked-a.txt", "", ActionRemoteDelete, SKService(), 2)))
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked-b.txt", "", ActionRemoteDelete, SKService(), 3)))

	require.NoError(t, deleteRetryWorkByWorkTx(ctx, store.db, RetryWorkKey{
		Path:       "delete-me.txt",
		ActionType: ActionUpload,
	}))
	require.NoError(t, markRetryWorkReadyTx(ctx, store.db, SKService().String(), []RetryWorkKey{
		{Path: "blocked-a.txt", ActionType: ActionRemoteDelete},
		{Path: "not-blocked.txt", ActionType: ActionUpload},
	}, now))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, SKService(), row.ScopeKey)
		if row.Path == "blocked-a.txt" {
			assert.False(t, row.Blocked, "a named row under the scope becomes ready")
			assert.Equal(t, now, row.NextRetryAt)
			continue
		}
		assert.True(t, row.Blocked, "a row not named stays blocked")
		assert.Zero(t, row.NextRetryAt)
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
		_, recordErr := store.RecordRetryWorkFailure(ctx, testRetryWorkFailure("bad-action.txt", "", actionType(-1)), func(int) time.Duration { return time.Minute })
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
	other = *testBlockedRetryWorkRow(other.Path, other.OldPath, other.ActionType, SKService(), 2)
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
	other = *testDelayedRetryWorkRow(other.Path, other.OldPath, other.ActionType, 2, time.Now().Add(time.Minute).UnixNano())
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
	row = *testDelayedRetryWorkRow(row.Path, row.OldPath, row.ActionType, 3, time.Now().Add(time.Minute).UnixNano())
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

// An empty condition_type is a transient failure and reads as no condition;
// a member reads back as itself; any other stored value fails the read
// naming the row, so status and admission never act on a condition no
// member names.
// Validates: R-2.5.6
func TestScanRetryWorkRow_ConditionTypeMustBeEmptyOrAMember(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	actionable := testDelayedRetryWorkRow("refused.txt", "", ActionUpload, 1, 0)
	actionable.ConditionType = issuePathTooLong
	require.NoError(t, store.UpsertRetryWork(ctx, actionable))
	require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow("transient.txt", "", ActionDownload, 1, 0)))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	byPath := make(map[string]IssueType, len(rows))
	for i := range rows {
		byPath[rows[i].Path] = rows[i].ConditionType
	}
	assert.Equal(t, map[string]IssueType{"refused.txt": issuePathTooLong, "transient.txt": ""}, byPath)

	_, err = store.rawDB().ExecContext(ctx,
		`UPDATE retry_work SET condition_type = ? WHERE path = ?`, "path_too_long_v2", "refused.txt")
	require.NoError(t, err)

	_, err = store.ListRetryWork(ctx)
	require.ErrorContains(t, err, `unknown issue type "path_too_long_v2"`)
	require.ErrorContains(t, err, "refused.txt")
}

// The write refuses a condition type the read would refuse, so a bad value
// never reaches the table.
// Validates: R-2.5.6
func TestUpsertRetryWork_RefusesAConditionTypeNoMemberHas(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	row := testDelayedRetryWorkRow("refused.txt", "", ActionUpload, 1, 0)
	row.ConditionType = IssueType("path_too_long_v2")
	require.ErrorContains(t, store.UpsertRetryWork(ctx, row), `unknown condition type "path_too_long_v2"`)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
