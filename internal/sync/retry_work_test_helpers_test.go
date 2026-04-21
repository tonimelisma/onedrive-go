package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func listObservationIssuesForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
) []ObservationIssueRow {
	t.Helper()

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)

	return rows
}

func actionableObservationIssuesForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
) []ObservationIssueRow {
	t.Helper()

	return listObservationIssuesForTest(t, store, ctx)
}

func listRetryWorkForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
) []RetryWorkRow {
	t.Helper()

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)

	return rows
}
