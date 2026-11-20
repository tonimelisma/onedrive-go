package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func testRetryWorkKey(path string, oldPath string, actionType ActionType) RetryWorkKey {
	return RetryWorkKey{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func testRetryWorkFailure(path string, oldPath string, actionType ActionType) *RetryWorkFailure {
	return &RetryWorkFailure{
		Work: testRetryWorkKey(path, oldPath, actionType),
	}
}

func testRetryWorkRow(path string, oldPath string, actionType ActionType) RetryWorkRow {
	return RetryWorkRow{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func listRetryWorkForTest(t *testing.T, store *SyncStore, ctx context.Context) []RetryWorkRow {
	t.Helper()

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	return rows
}

func actionableObservationIssuesForTest(t *testing.T, store *SyncStore, ctx context.Context) []ObservationIssueRow {
	t.Helper()

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	return rows
}
