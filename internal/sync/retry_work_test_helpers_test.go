package sync

import (
	"context"
	"testing"
	"time"

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

func visibleConditionCountForTest(t *testing.T, store *SyncStore, ctx context.Context) int {
	t.Helper()

	summary, err := store.ReadVisibleConditionSummary(ctx)
	require.NoError(t, err)

	return summary.VisibleTotal()
}

func readyRetryWorkForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	now time.Time,
) []RetryWorkRow {
	t.Helper()

	rows, err := store.ListRetryWorkReady(ctx, now)
	require.NoError(t, err)

	return rows
}
