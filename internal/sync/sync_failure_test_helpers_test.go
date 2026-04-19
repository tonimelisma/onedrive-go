package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func listSyncFailuresForTest(t *testing.T, store *SyncStore, ctx context.Context) []SyncFailureRow {
	t.Helper()

	rows, err := store.ListSyncFailures(ctx)
	require.NoError(t, err)

	return rows
}

func filterSyncFailuresForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	keep func(SyncFailureRow) bool,
) []SyncFailureRow {
	t.Helper()

	rows := listSyncFailuresForTest(t, store, ctx)
	filtered := make([]SyncFailureRow, 0, len(rows))
	for i := range rows {
		row := rows[i]
		if keep(row) {
			filtered = append(filtered, row)
		}
	}

	return filtered
}

func actionableSyncFailuresForTest(t *testing.T, store *SyncStore, ctx context.Context) []SyncFailureRow {
	t.Helper()

	return filterSyncFailuresForTest(t, store, ctx, func(row SyncFailureRow) bool {
		return row.Category == CategoryActionable
	})
}

func syncFailuresByIssueTypeForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	issueType string,
) []SyncFailureRow {
	t.Helper()

	return filterSyncFailuresForTest(t, store, ctx, func(row SyncFailureRow) bool {
		return row.IssueType == issueType
	})
}

func syncFailureByPathForTest(
	t *testing.T,
	store *SyncStore,
	ctx context.Context,
	path string,
) (*SyncFailureRow, bool) {
	t.Helper()

	rows := filterSyncFailuresForTest(t, store, ctx, func(row SyncFailureRow) bool {
		return row.Path == path
	})
	if len(rows) == 0 {
		return nil, false
	}

	return &rows[0], true
}

func visibleIssueCountForTest(t *testing.T, store *SyncStore, ctx context.Context) int {
	t.Helper()

	summary, err := store.ReadVisibleIssueSummary(ctx)
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
