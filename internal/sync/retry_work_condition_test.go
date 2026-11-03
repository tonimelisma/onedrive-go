package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.71
//
// A row recorded for a condition only the user can clear keeps that
// condition, is read back by the actionable query from its first attempt,
// and never counts as "Retrying": that count is for transient failures.
func TestRecordRetryWorkFailure_KeepsTheConditionItWasRefusedFor(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	delay := func(int) time.Duration { return time.Minute }

	refused := &RetryWorkFailure{
		Work:          RetryWorkKey{Path: "faults/deep", ActionType: ActionFolderCreate},
		ConditionType: issuePathTooLong,
	}
	transient := &RetryWorkFailure{
		Work: RetryWorkKey{Path: "docs/a.txt", ActionType: ActionUpload},
	}
	for range 3 {
		row, err := store.RecordRetryWorkFailure(ctx, refused, delay)
		require.NoError(t, err)
		assert.Equal(t, issuePathTooLong, row.ConditionType)
		_, err = store.RecordRetryWorkFailure(ctx, transient, delay)
		require.NoError(t, err)
	}

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	byPath := make(map[string]RetryWorkRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}
	assert.Equal(t, issuePathTooLong, byPath["faults/deep"].ConditionType, "the condition survives the round trip")
	assert.Empty(t, byPath["docs/a.txt"].ConditionType)

	actionable, err := queryActionableRetryWorkRowsWithRunner(ctx, store.db)
	require.NoError(t, err)
	require.Len(t, actionable, 1)
	assert.Equal(t, "faults/deep", actionable[0].Path)

	retrying, err := queryRetryingRetryWorkRowsWithRunner(ctx, store.db)
	require.NoError(t, err)
	require.Len(t, retrying, 1, "only the transient failure is retrying work")
	assert.Equal(t, "docs/a.txt", retrying[0].Path)

	count, err := store.CountRetryingWork(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the refused folder is an issue, not a retry")
}

// Validates: R-2.10.71
//
// Status shows the work OneDrive refused under the same condition as the
// scanner's own findings: a folder the server refused as too long joins the
// "Path too long" group next to the path the scanner refused, and a download
// into a folder that cannot be written is a local write issue, not an
// anonymous retry.
func TestProjectStoredConditionGroups_ActionableRetryWorkJoinsItsCondition(t *testing.T) {
	t.Parallel()

	snapshot := &DriveStatusSnapshot{
		ObservationIssues: []ObservationIssueRow{
			{Path: "faults/seven-levels", IssueType: issuePathTooLong},
		},
		ActionableRetryWork: []RetryWorkRow{
			{Path: "faults/six-levels", ActionType: ActionFolderCreate, ConditionType: issuePathTooLong, AttemptCount: 21},
			{Path: "perm/ro/into-ro.bin", ActionType: ActionDownload, ConditionType: issueLocalWriteDenied, AttemptCount: 1},
		},
	}

	groups := ProjectStoredConditionGroups(snapshot)
	byKey := make(map[ConditionKey]storedConditionGroup, len(groups))
	for _, group := range groups {
		byKey[group.ConditionKey] = group
	}

	tooLong, ok := byKey[ConditionPathTooLong]
	require.True(t, ok, "groups: %+v", groups)
	assert.Equal(t, 2, tooLong.Count)
	assert.Equal(t, []string{"faults/seven-levels", "faults/six-levels"}, tooLong.Paths)

	writeDenied, ok := byKey[ConditionLocalWriteDenied]
	require.True(t, ok, "groups: %+v", groups)
	assert.Equal(t, 1, writeDenied.Count)
	assert.Equal(t, []string{"perm/ro/into-ro.bin"}, writeDenied.Paths)
}
