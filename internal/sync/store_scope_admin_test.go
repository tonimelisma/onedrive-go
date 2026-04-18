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

func insertFailureForStoreScopeTest(
	t *testing.T,
	store *SyncStore,
	path string,
	driveID driveid.ID,
	category FailureCategory,
	role FailureRole,
	issueType string,
	scopeKey ScopeKey,
	nextRetryAt any,
) {
	t.Helper()

	now := time.Now().UnixNano()
	require.NoError(t, store.CommitObservationCursor(t.Context(), driveID, ""))
	_, err := store.DB().ExecContext(t.Context(),
		`INSERT INTO sync_failures (
			path, direction, action_type, category, failure_role, issue_type,
			item_id, failure_count, next_retry_at, last_error, http_status, first_seen_at, last_seen_at,
			file_size, local_hash, scope_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		path,
		DirectionUpload,
		ActionUpload,
		category,
		role,
		issueType,
		nil,
		1,
		nextRetryAt,
		"boom",
		nil,
		now,
		now,
		nil,
		nil,
		scopeKey.String(),
	)
	require.NoError(t, err)
}

func failureRowCountForStoreScopeTest(t *testing.T, store *SyncStore, path string) int {
	t.Helper()

	var count int
	err := store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sync_failures WHERE path = ?`, path,
	).Scan(&count)
	require.NoError(t, err)

	return count
}

func syncStorePathForStoreScopeTest(t *testing.T, store *SyncStore) string {
	t.Helper()

	rows, err := store.DB().QueryContext(t.Context(), "PRAGMA database_list")
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
func TestSyncStore_ClearHeldRetryWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemote("Shared/Docs")

	insertFailureForStoreScopeTest(t, store, "held.txt", driveID, CategoryTransient, FailureRoleHeld, IssueSharedFolderBlocked, scopeKey, nil)
	require.NoError(t, store.ClearHeldRetryWork(t.Context(), RetryWorkKey{
		Path:       "held.txt",
		ActionType: ActionUpload,
	}, scopeKey))
	assert.Zero(t, failureRowCountForStoreScopeTest(t, store, "held.txt"))
}

// Validates: R-2.10.11
func TestSyncStore_ResetRetryTimesForScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKService()
	now := time.Now()
	pastRetry := now.Add(-time.Minute).UnixNano()
	futureRetry := now.Add(time.Hour).UnixNano()

	insertFailureForStoreScopeTest(t, store, "future.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, scopeKey, futureRetry)
	insertFailureForStoreScopeTest(t, store, "past.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, scopeKey, pastRetry)
	insertFailureForStoreScopeTest(t, store, "other-scope.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, SKPermRemote("Shared/Elsewhere"), futureRetry)

	require.NoError(t, store.ResetRetryTimesForScope(t.Context(), scopeKey, now))

	var gotFuture int64
	err := store.DB().QueryRowContext(t.Context(),
		`SELECT next_retry_at FROM sync_failures WHERE path = ?`, "future.txt",
	).Scan(&gotFuture)
	require.NoError(t, err)
	assert.Equal(t, now.UnixNano(), gotFuture)

	var gotPast int64
	err = store.DB().QueryRowContext(t.Context(),
		`SELECT next_retry_at FROM sync_failures WHERE path = ?`, "past.txt",
	).Scan(&gotPast)
	require.NoError(t, err)
	assert.Equal(t, pastRetry, gotPast)

	var gotOther int64
	err = store.DB().QueryRowContext(t.Context(),
		`SELECT next_retry_at FROM sync_failures WHERE path = ?`, "other-scope.txt",
	).Scan(&gotOther)
	require.NoError(t, err)
	assert.Equal(t, futureRetry, gotOther)
}

// Validates: R-2.3.10, R-2.10.4
func TestReadDriveStatusSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemote("Shared/Docs")

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
	insertFailureForStoreScopeTest(t, store, "bad:name.txt", driveID, CategoryActionable, FailureRoleItem, IssueInvalidFilename, ScopeKey{}, nil)
	require.NoError(t, store.UpsertScopeBlock(t.Context(), &ScopeBlock{
		Key:           scopeKey,
		IssueType:     IssueSharedFolderBlocked,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(1, 0),
		TrialInterval: time.Second,
		NextTrialAt:   time.Unix(2, 0),
	}))

	dbPath := syncStorePathForStoreScopeTest(t, store)
	snapshot, err := ReadDriveStatusSnapshot(t.Context(), dbPath, false, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.BaselineEntryCount)
	assert.Equal(t, 3, snapshot.RunStatus.LastSucceededCount)
	require.Len(t, snapshot.IssueGroups, 1)
	assert.Equal(t, SummaryInvalidFilename, snapshot.IssueGroups[0].SummaryKey)
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
