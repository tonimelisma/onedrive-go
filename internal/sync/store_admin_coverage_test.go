package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const sqliteMainDatabaseName = "main"

func insertFailureForCoverageTest(
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

func failureRowCountForCoverageTest(t *testing.T, store *SyncStore, path string) int {
	t.Helper()

	var count int
	err := store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sync_failures WHERE path = ?`, path,
	).Scan(&count)
	require.NoError(t, err)

	return count
}

func syncStorePathForCoverageTest(t *testing.T, store *SyncStore) string {
	t.Helper()

	rows, err := store.DB().QueryContext(t.Context(), "PRAGMA database_list")
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name string
		var file string
		require.NoError(t, rows.Scan(&seq, &name, &file))
		if name == sqliteMainDatabaseName {
			require.NotEmpty(t, file)
			return file
		}
	}

	require.NoError(t, rows.Err())
	require.FailNow(t, "main database path not found")
	return ""
}

// Validates: R-2.5.2
func TestSyncStore_FailureAdminMutations(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemote("Shared/Docs")
	futureRetry := time.Now().Add(time.Hour).UnixNano()

	insertFailureForCoverageTest(t, store, "retry.txt", driveID, CategoryTransient, FailureRoleItem, IssueInvalidFilename, ScopeKey{}, futureRetry)
	insertFailureForCoverageTest(t, store, "held.txt", driveID, CategoryTransient, FailureRoleHeld, IssueSharedFolderBlocked, scopeKey, nil)

	count, err := store.SyncFailureCount(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	require.NoError(t, store.ResetFailure(t.Context(), "retry.txt"))
	assert.Zero(t, failureRowCountForCoverageTest(t, store, "retry.txt"))

	require.NoError(t, store.ResetFailure(t.Context(), "held.txt"))
	assert.Equal(t, 1, failureRowCountForCoverageTest(t, store, "held.txt"))

	insertFailureForCoverageTest(t, store, "transient.txt", driveID, CategoryTransient, FailureRoleItem, IssueInvalidFilename, ScopeKey{}, futureRetry)
	insertFailureForCoverageTest(t, store, "actionable.txt", driveID, CategoryActionable, FailureRoleItem, IssueInvalidFilename, ScopeKey{}, nil)

	require.NoError(t, store.ResetAllFailures(t.Context()))
	assert.Zero(t, failureRowCountForCoverageTest(t, store, "transient.txt"))
	assert.Equal(t, 1, failureRowCountForCoverageTest(t, store, "actionable.txt"))

	require.NoError(t, store.ClearSyncFailureByPath(t.Context(), "held.txt"))
	assert.Zero(t, failureRowCountForCoverageTest(t, store, "held.txt"))
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

	insertFailureForCoverageTest(t, store, "future.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, scopeKey, futureRetry)
	insertFailureForCoverageTest(t, store, "past.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, scopeKey, pastRetry)
	insertFailureForCoverageTest(t, store, "other-scope.txt", driveID, CategoryTransient, FailureRoleItem, IssueServiceOutage, SKPermRemote("Shared/Elsewhere"), futureRetry)

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
func TestReadDriveStatusSnapshotAndScopeBlockHelpers(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	scopeKey := SKPermRemote("Shared/Docs")

	require.NoError(t, store.WriteSyncMetadata(t.Context(), &SyncMetadata{
		Duration:  2 * time.Second,
		Succeeded: 3,
		Failed:    1,
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
	insertFailureForCoverageTest(t, store, "bad:name.txt", driveID, CategoryActionable, FailureRoleItem, IssueInvalidFilename, ScopeKey{}, nil)
	require.NoError(t, store.UpsertScopeBlock(t.Context(), &ScopeBlock{
		Key:           scopeKey,
		IssueType:     IssueSharedFolderBlocked,
		TimingSource:  ScopeTimingBackoff,
		BlockedAt:     time.Unix(1, 0),
		TrialInterval: time.Second,
		NextTrialAt:   time.Unix(2, 0),
	}))

	dbPath := syncStorePathForCoverageTest(t, store)
	snapshot, err := ReadDriveStatusSnapshot(t.Context(), dbPath, false, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.BaselineEntryCount)
	assert.Equal(t, "3", snapshot.SyncMetadata["last_sync_succeeded"])
	require.Len(t, snapshot.IssueGroups, 1)
	assert.Equal(t, SummaryInvalidFilename, snapshot.IssueGroups[0].SummaryKey)

	hasScope, err := HasScopeBlockAtPath(context.Background(), dbPath, scopeKey, testLogger(t))
	require.NoError(t, err)
	assert.True(t, hasScope)
}

// Validates: R-2.2
func TestSyncStore_RemoteStateLookupAndDeltaTokenDeletion(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-lookup",
		ParentID: "root",
		Path:     "docs/lookup.txt",
		ItemType: ItemTypeFile,
		Hash:     "lookup-hash",
		Size:     55,
		Mtime:    77,
		ETag:     "etag-lookup",
	}}, "delta-to-delete", driveID))

	row, found, err := store.GetRemoteStateByID(t.Context(), driveID, "item-lookup")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "docs/lookup.txt", row.Path)
	assert.Equal(t, "lookup-hash", row.Hash)

	row, found, err = store.GetRemoteStateByID(t.Context(), driveID, "missing-item")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, row)

	require.NoError(t, store.ClearObservationCursor(t.Context()))

	token := readObservationCursorForTest(t, store, t.Context(), driveID.String())
	assert.Empty(t, token)
}
