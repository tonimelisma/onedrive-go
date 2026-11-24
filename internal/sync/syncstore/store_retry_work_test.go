package syncstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.1.3
func TestUpsertRetryWorkAndPruneToCurrentActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("keep.txt", "", syncdomain.ActionUpload, 2, 10)))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("drop.txt", "", syncdomain.ActionDownload, 1, 20)))

	pruned, err := store.PruneRetryWorkToCurrentActions(ctx, []syncdomain.RetryWorkKey{
		{Path: "keep.txt", ActionType: syncdomain.ActionUpload},
	})
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.RetryWorkKey{{Path: "drop.txt", ActionType: syncdomain.ActionDownload}}, pruned,
		"the prune names the work it deleted")

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "keep.txt", rows[0].Path)
	assert.Equal(t, syncdomain.ActionUpload, rows[0].ActionType)
}

// Validates: R-2.10.33
func TestRetryWorkPruneDistinguishesOldPathSemanticWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("dest.txt", "src-a.txt", syncdomain.ActionRemoteMove, 1, 1)))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("dest.txt", "src-b.txt", syncdomain.ActionRemoteMove, 1, 1)))

	_, err := store.PruneRetryWorkToCurrentActions(ctx, []syncdomain.RetryWorkKey{
		{Path: "dest.txt", OldPath: "src-b.txt", ActionType: syncdomain.ActionRemoteMove},
	})
	require.NoError(t, err)

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

	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("retry-now.txt", "", syncdomain.ActionDownload, 1, now.UnixNano())))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("retry-later.txt", "", syncdomain.ActionUpload, 1, now.Add(time.Hour).UnixNano())))
	seedBlockScopeForTest(t, store, syncdomain.SKService())
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked.txt", "", syncdomain.ActionRemoteDelete, syncdomain.SKService(), 2)))

	blocked, err := store.ListBlockedRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Equal(t, "blocked.txt", blocked[0].Path)
	assert.Equal(t, syncdomain.SKService(), blocked[0].ScopeKey)

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

	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("retrying-a.txt", "", syncdomain.ActionUpload, 3, 10)))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("retrying-b.txt", "", syncdomain.ActionDownload, 4, 20)))
	seedBlockScopeForTest(t, store, syncdomain.SKService())
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked.txt", "", syncdomain.ActionRemoteDelete, syncdomain.SKService(), 7)))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("fresh.txt", "", syncdomain.ActionUpload, 2, 30)))

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

	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("delete-me.txt", "", syncdomain.ActionUpload, 1, 1)))
	seedBlockScopeForTest(t, store, syncdomain.SKService())
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked-a.txt", "", syncdomain.ActionRemoteDelete, syncdomain.SKService(), 2)))
	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked-b.txt", "", syncdomain.ActionRemoteDelete, syncdomain.SKService(), 3)))

	require.NoError(t, deleteRetryWorkByWorkTx(ctx, store.db, syncdomain.RetryWorkKey{
		Path:       "delete-me.txt",
		ActionType: syncdomain.ActionUpload,
	}))
	released, err := markRetryWorkReadyTx(ctx, store.db, syncdomain.SKService().String(), []syncdomain.RetryWorkKey{
		{Path: "blocked-a.txt", ActionType: syncdomain.ActionRemoteDelete},
		{Path: "not-blocked.txt", ActionType: syncdomain.ActionUpload},
	}, now)
	require.NoError(t, err)
	require.Len(t, released, 1, "only the row blocked under the scope is released and returned")
	assert.Equal(t, "blocked-a.txt", released[0].Path)
	assert.False(t, released[0].Blocked, "returned as it is after the release")
	assert.Equal(t, now, released[0].NextRetryAt)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, syncdomain.SKService(), row.ScopeKey)
		if row.Path == "blocked-a.txt" {
			assert.False(t, row.Blocked, "a named row under the scope becomes ready")
			assert.Equal(t, now, row.NextRetryAt)
			continue
		}
		assert.True(t, row.Blocked, "a row not named stays blocked")
		assert.Zero(t, row.NextRetryAt)
	}

	deleted, err := deleteBlockedRetryWorkByScopeTx(ctx, store.db, syncdomain.SKService().String())
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.RetryWorkKey{{Path: "blocked-b.txt", ActionType: syncdomain.ActionRemoteDelete}}, deleted,
		"only the row still blocked under the scope is deleted")

	rows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "blocked-a.txt", rows[0].Path, "the released row naming the scope stays")
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
		_, recordErr := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("", "", syncdomain.ActionUpload), func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("bad-action.txt", "", syncdomain.ActionType(-1)), func(int) time.Duration { return time.Minute })
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid action type")

	err = func() error {
		_, recordErr := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("needs-delay.txt", "", syncdomain.ActionUpload), nil)
		return recordErr
	}()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires delay function")

	err = func() error {
		_, recordErr := store.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("blocked-without-scope.txt", "", syncdomain.ActionUpload), syncdomain.ScopeKey{}, nil)
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

	_, err := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("retry.txt", "", syncdomain.ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	seedBlockScopeForTest(t, store, syncdomain.SKService())
	_, err = store.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionRemoteDelete), syncdomain.SKService(), nil)
	require.NoError(t, err)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byPath := make(map[string]syncdomain.RetryWorkRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}

	assert.False(t, byPath["retry.txt"].Blocked)
	assert.Equal(t, now.Add(time.Minute).UnixNano(), byPath["retry.txt"].NextRetryAt)

	assert.True(t, byPath["blocked.txt"].Blocked)
	assert.Equal(t, int64(0), byPath["blocked.txt"].NextRetryAt)
	assert.Equal(t, syncdomain.SKService(), byPath["blocked.txt"].ScopeKey)
}

// Validates: R-2.10.33
func TestRecordRetryWorkFailure_PreservesMoveOldPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("dest.txt", "src.txt", syncdomain.ActionRemoteMove), func(int) time.Duration { return time.Minute })
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

	seedBlockScopeForTest(t, store, syncdomain.SKService())
	_, err := store.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), syncdomain.SKService(), nil)
	require.NoError(t, err)

	other := syncfixture.TestRetryWorkRow("blocked.txt", "old.txt", syncdomain.ActionRemoteMove)
	other = *testBlockedRetryWorkRow(other.Path, other.OldPath, other.ActionType, syncdomain.SKService(), 2)
	require.NoError(t, store.UpsertRetryWork(ctx, &other))

	cleared, err := store.ClearBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: syncdomain.ActionUpload,
	}, syncdomain.SKService())
	require.NoError(t, err)
	assert.True(t, cleared, "the blocked row was cleared")
	cleared, err = store.ClearBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: syncdomain.ActionUpload,
	}, syncdomain.SKService())
	require.NoError(t, err)
	assert.False(t, cleared, "nothing is left to clear")

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "blocked.txt", rows[0].Path)
	assert.Equal(t, syncdomain.ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

// Validates: R-2.10.33
func TestResolveRetryWork_ReturnsAndDeletesMatchingWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("docs/report.txt", "", syncdomain.ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	row, found, err := store.ResolveRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: syncdomain.ActionUpload,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	assert.Equal(t, "docs/report.txt", row.Path)
	assert.Equal(t, syncdomain.ActionUpload, row.ActionType)
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

	_, err := store.RecordRetryWorkFailure(ctx, syncfixture.TestRetryWorkFailure("docs/report.txt", "", syncdomain.ActionUpload), func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	other := syncfixture.TestRetryWorkRow("docs/report.txt", "old.txt", syncdomain.ActionRemoteMove)
	other = *syncfixture.TestDelayedRetryWorkRow(other.Path, other.OldPath, other.ActionType, 2, time.Now().Add(time.Minute).UnixNano())
	require.NoError(t, store.UpsertRetryWork(ctx, &other))

	_, found, err := store.ResolveRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "docs/report.txt",
		ActionType: syncdomain.ActionUpload,
	})
	require.NoError(t, err)
	require.True(t, found)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, syncdomain.ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

func TestResolveRetryWork_DeletesRetryWorkWithoutIssueRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	row := syncfixture.TestRetryWorkRow("docs/report.txt", "old.txt", syncdomain.ActionRemoteMove)
	row = *syncfixture.TestDelayedRetryWorkRow(row.Path, row.OldPath, row.ActionType, 3, time.Now().Add(time.Minute).UnixNano())
	require.NoError(t, store.UpsertRetryWork(ctx, &row))

	resolved, found, err := store.ResolveRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "docs/report.txt",
		OldPath:    "old.txt",
		ActionType: syncdomain.ActionRemoteMove,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, resolved)
	assert.Equal(t, "docs/report.txt", resolved.Path)
	assert.Equal(t, "old.txt", resolved.OldPath)
	assert.Equal(t, syncdomain.ActionRemoteMove, resolved.ActionType)
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

	actionable := syncfixture.TestDelayedRetryWorkRow("refused.txt", "", syncdomain.ActionUpload, 1, 1)
	actionable.ConditionType = syncdomain.IssuePathTooLong
	require.NoError(t, store.UpsertRetryWork(ctx, actionable))
	require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow("transient.txt", "", syncdomain.ActionDownload, 1, 1)))

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	byPath := make(map[string]syncdomain.IssueType, len(rows))
	for i := range rows {
		byPath[rows[i].Path] = rows[i].ConditionType
	}
	assert.Equal(t, map[string]syncdomain.IssueType{"refused.txt": syncdomain.IssuePathTooLong, "transient.txt": ""}, byPath)

	_, err = store.db.ExecContext(ctx,
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

	row := syncfixture.TestDelayedRetryWorkRow("refused.txt", "", syncdomain.ActionUpload, 1, 1)
	row.ConditionType = syncdomain.IssueType("path_too_long_v2")
	require.ErrorContains(t, store.UpsertRetryWork(ctx, row), `unknown condition type "path_too_long_v2"`)

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
