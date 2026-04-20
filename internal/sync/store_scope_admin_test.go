package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const storeScopeMainDatabaseName = "main"

func retryWorkRowCountForStoreScopeTest(t *testing.T, store *SyncStore, path string) int {
	t.Helper()

	var count int
	err := store.rawDB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM retry_work WHERE path = ?`, path,
	).Scan(&count)
	require.NoError(t, err)

	return count
}

func syncStorePathForStoreScopeTest(t *testing.T, store *SyncStore) string {
	t.Helper()

	rows, err := store.rawDB().QueryContext(t.Context(), "PRAGMA database_list")
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name string
		var file string
		require.NoError(t, rows.Scan(&seq, &name, &file))
		if name == storeScopeMainDatabaseName {
			require.NotEmpty(t, file)
			return file
		}
	}

	require.NoError(t, rows.Err())
	require.FailNow(t, "main database path not found")
	return ""
}

// Validates: R-2.5.2
func TestSyncStore_ClearBlockedRetryWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	_, err := store.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   scopeKey,
		LastError:  "blocked",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, store.ClearBlockedRetryWork(t.Context(), RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, scopeKey))
	assert.Zero(t, retryWorkRowCountForStoreScopeTest(t, store, "blocked.txt"))
}

// Validates: R-2.3.10, R-2.10.4
func TestReadDriveStatusSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, store.WriteSyncRunStatus(t.Context(), &SyncRunReport{
		CompletedAt: time.Date(2026, 4, 3, 10, 30, 0, 0, time.UTC),
		Duration:    2 * time.Second,
		Succeeded:   3,
		Failed:      1,
	}))
	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "ok.txt",
		DriveID:         driveID,
		ItemID:          "item-ok",
		ItemType:        ItemTypeFile,
		LocalHash:       "abc123",
		RemoteHash:      "abc123",
		LocalSize:       12,
		LocalSizeKnown:  true,
		RemoteSize:      12,
		RemoteSizeKnown: true,
		LocalMtime:      1700000000,
		RemoteMtime:     1700000000,
	}))
	require.NoError(t, store.UpsertObservationIssue(t.Context(), &ObservationIssue{
		Path:       "bad:name.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		IssueType:  IssueInvalidFilename,
		Error:      "invalid filename",
	}))
	require.NoError(t, store.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		IssueType:     IssueRemoteWriteDenied,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(1, 0),
		TrialInterval: time.Second,
		NextTrialAt:   time.Unix(2, 0),
	}))
	_, err := store.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:       "Shared/Docs/a.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   scopeKey,
		LastError:  "blocked",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	dbPath := syncStorePathForStoreScopeTest(t, store)
	snapshot, err := ReadDriveStatusSnapshot(t.Context(), dbPath, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.BaselineEntryCount)
	assert.Equal(t, 3, snapshot.RunStatus.LastSucceededCount)
	require.Len(t, snapshot.ObservationIssues, 1)
	assert.Equal(t, "bad:name.txt", snapshot.ObservationIssues[0].Path)
	require.Len(t, snapshot.BlockScopes, 1)
	assert.Equal(t, scopeKey, snapshot.BlockScopes[0].Key)
	require.Len(t, snapshot.BlockedRetryWork, 1)
	assert.Equal(t, "Shared/Docs/a.txt", snapshot.BlockedRetryWork[0].Path)
}

func TestFinalizeInspectorRead_PreservesSuccessfulReadOnCloseError(t *testing.T) {
	t.Parallel()

	result, err := finalizeInspectorRead("state.db", newTestLogger(t), true, nil, assert.AnError)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestFinalizeInspectorRead_JoinsReadAndCloseErrors(t *testing.T) {
	t.Parallel()

	readErr := assert.AnError
	closeErr := context.Canceled

	_, err := finalizeInspectorRead("state.db", newTestLogger(t), false, readErr, closeErr)
	require.Error(t, err)
	require.ErrorIs(t, err, readErr)
	require.ErrorIs(t, err, closeErr)
}

// Validates: R-2.5.2, R-2.10.8
func TestSyncStore_ReleaseScope_MakesBlockedRetryWorkReadyAndPreservesObservationIssues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	scopeKey := SKPermRemoteWrite("Shared/Docs")
	now := time.Date(2026, 4, 19, 9, 30, 0, 0, time.UTC)

	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:           scopeKey,
		IssueType:     IssueRemoteWriteDenied,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   now,
	}))
	seedObservationIssueForTest(t, store, "Shared/Docs/bad.txt", IssueRemoteWriteDenied, scopeKey)
	seedObservationIssueForTest(t, store, "other/problem.txt", IssueInvalidFilename, ScopeKey{})
	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "Shared/Docs/file.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   scopeKey,
		LastError:  "blocked",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, store.ReleaseScope(ctx, scopeKey, now))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Shared/Docs/bad.txt", "other/problem.txt"}, []string{rows[0].Path, rows[1].Path})

	retryRows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/Docs/file.txt", retryRows[0].Path)
	assert.False(t, retryRows[0].Blocked)
	assert.Equal(t, now.UnixNano(), retryRows[0].NextRetryAt)

	ready, err := store.ListRetryWorkReady(ctx, now)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "Shared/Docs/file.txt", ready[0].Path)
}

// Validates: R-2.5.2, R-2.10.8
func TestSyncStore_DiscardScope_DeletesBlockedRetryWorkAndPreservesObservationIssues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:          scopeKey,
		IssueType:    IssueRemoteWriteDenied,
		TimingSource: ScopeTimingNone,
		BlockedAt:    time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC),
	}))
	seedObservationIssueForTest(t, store, "Shared/Docs/bad.txt", IssueRemoteWriteDenied, scopeKey)
	seedObservationIssueForTest(t, store, "keep.txt", IssueInvalidFilename, ScopeKey{})
	_, err := store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "Shared/Docs/file.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   scopeKey,
		LastError:  "blocked",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, store.DiscardScope(ctx, scopeKey))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Shared/Docs/bad.txt", "keep.txt"}, []string{rows[0].Path, rows[1].Path})

	retryRows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, retryRows)
}

func TestSyncStore_DiscardScope_RejectsZeroScopeKey(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	err := store.DiscardScope(t.Context(), ScopeKey{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scope key")
}
