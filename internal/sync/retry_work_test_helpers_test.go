package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func listRetryWorkForTest(t *testing.T, store *syncstore.SyncStore, ctx context.Context) []syncdomain.RetryWorkRow {
	t.Helper()

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	return rows
}

func actionableObservationIssuesForTest(t *testing.T, store *syncstore.SyncStore, ctx context.Context) []syncdomain.ObservationIssueRow {
	t.Helper()

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	return rows
}
