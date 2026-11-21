package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const storeScopeMainDatabaseName = "main"

func retryWorkRowCountForStoreScopeTest(t *testing.T, store *SyncStore, path string) int {
	t.Helper()

	var count int
	err := store.RawDBForTest().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM retry_work WHERE path = ?`, path,
	).Scan(&count)
	require.NoError(t, err)

	return count
}

func syncStorePathForStoreScopeTest(t *testing.T, store *SyncStore) string {
	t.Helper()

	rows, err := store.RawDBForTest().QueryContext(t.Context(), "PRAGMA database_list")
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
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")
	seedBlockScopeForTest(t, store, scopeKey)

	_, err := store.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)

	cleared, err := store.ClearBlockedRetryWork(t.Context(), syncdomain.RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey)
	require.NoError(t, err)
	assert.True(t, cleared)
	assert.Zero(t, retryWorkRowCountForStoreScopeTest(t, store, "blocked.txt"))
}

// Validates: R-2.3.10, R-2.10.4
func TestReadDriveStatusSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, store.CommitMutation(t.Context(), &syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "ok.txt",
		DriveID:         driveID,
		ItemID:          "item-ok",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "abc123",
		RemoteHash:      "abc123",
		LocalSize:       12,
		LocalSizeKnown:  true,
		RemoteSize:      12,
		RemoteSizeKnown: true,
		LocalMtime:      1700000000,
		RemoteMtime:     1700000000,
	}))
	seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "bad:name.txt",
		DriveID:   driveID,
		IssueType: syncdomain.IssueInvalidFilename,
	})
	require.NoError(t, store.UpsertBlockScope(t.Context(), syncfixture.TestBlockScope(scopeKey, time.Second, time.Unix(2, 0))))
	_, err := store.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("Shared/Docs/a.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)

	dbPath := syncStorePathForStoreScopeTest(t, store)
	snapshot, err := ReadDriveStatusSnapshot(t.Context(), dbPath, syncfixture.TestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.BaselineFileCount)
	require.Len(t, snapshot.ObservationIssues, 1)
	assert.Equal(t, "bad:name.txt", snapshot.ObservationIssues[0].Path)
	require.Len(t, snapshot.BlockScopes, 1)
	assert.Equal(t, scopeKey, snapshot.BlockScopes[0].Key)
	require.Len(t, snapshot.BlockedRetryWork, 1)
	assert.Equal(t, "Shared/Docs/a.txt", snapshot.BlockedRetryWork[0].Path)
}

// Validates: R-2.10.48
func TestReadDriveStatusSnapshot_RemoteDriftUsesPlannerContentTest(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)

	// Baseline never records a folder mtime; Graph always reports one.
	require.NoError(t, store.CommitMutation(t.Context(), &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionFolderCreate,
		Success:  true,
		Path:     "Docs",
		DriveID:  driveID,
		ItemID:   "item-docs",
		ItemType: syncdomain.ItemTypeFolder,
	}))
	for _, file := range []struct{ path, id, hash string }{
		{path: "same.txt", id: "item-same", hash: "abc"},
		{path: "changed.txt", id: "item-changed", hash: "abc"},
	} {
		require.NoError(t, store.CommitMutation(t.Context(), &syncdomain.BaselineMutation{
			Action:          syncdomain.ActionDownload,
			Success:         true,
			Path:            file.path,
			DriveID:         driveID,
			ItemID:          file.id,
			ItemType:        syncdomain.ItemTypeFile,
			LocalHash:       file.hash,
			RemoteHash:      file.hash,
			LocalSize:       3,
			LocalSizeKnown:  true,
			RemoteSize:      3,
			RemoteSizeKnown: true,
			LocalMtime:      1700000000,
			RemoteMtime:     1700000000,
		}))
	}

	// The commits above wrote matching remote_state rows; replace them with
	// what a later remote observation reports.
	_, err := store.RawDBForTest().ExecContext(t.Context(), `
		INSERT OR REPLACE INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-docs', 'Docs', 'folder', NULL, NULL, 1777605693000000000),
			('item-same', 'same.txt', 'file', 'abc', 3, 1700000999),
			('item-changed', 'changed.txt', 'file', 'def', 3, 1700000000)`)
	require.NoError(t, err)

	snapshot, err := ReadDriveStatusSnapshot(t.Context(), syncStorePathForStoreScopeTest(t, store), syncfixture.TestLogger(t))
	require.NoError(t, err)
	// Only changed.txt differs: the folder's mtime and same.txt's mtime are
	// not changes when the planner would not act on them.
	assert.Equal(t, 1, snapshot.RemoteDriftItems)
}

// Validates: R-2.1.3, R-2.10.4
func TestReadPathTruthStatus_DerivesUnavailableTruthFromDurableAuthorities(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)

	seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "bad:name.txt",
		DriveID:   driveID,
		IssueType: syncdomain.IssueInvalidFilename,
	})
	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(syncdomain.SKPermRemoteWrite("Private"), time.Minute, time.Unix(61, 0))))

	dbPath := syncStorePathForStoreScopeTest(t, store)
	statuses, err := readPathTruthStatus(ctx, dbPath, syncfixture.TestLogger(t), []string{
		"bad:name.txt",
		"Private/sub/file.txt",
	})
	require.NoError(t, err)
	require.Len(t, statuses, 2)

	assert.Equal(t, syncdomain.TruthAvailabilityBlockedObservationIssue, statuses["bad:name.txt"].Local.Availability)
	assert.Equal(t, syncdomain.IssueInvalidFilename, statuses["bad:name.txt"].Local.IssueType)
	assert.Equal(t, syncdomain.TruthAvailabilityAvailable, statuses["Private/sub/file.txt"].Local.Availability)
	assert.True(t, statuses["Private/sub/file.txt"].Local.ScopeKey.IsZero())
}

// Validates: R-2.1.3, R-2.10.4
func TestReadPathTruthStatus_DerivesReadBoundaryDescendantsFromObservationIssues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	scopeKey := syncdomain.SKPermLocalRead("Private")

	seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
		ScopeKey:  scopeKey,
	})

	dbPath := syncStorePathForStoreScopeTest(t, store)
	statuses, err := readPathTruthStatus(ctx, dbPath, syncfixture.TestLogger(t), []string{
		"Private/sub/file.txt",
	})
	require.NoError(t, err)
	require.Len(t, statuses, 1)

	assert.Equal(t, syncdomain.TruthAvailabilityBlockedObservationIssue, statuses["Private/sub/file.txt"].Local.Availability)
	assert.Equal(t, syncdomain.PathTruthSourceObservationIssue, statuses["Private/sub/file.txt"].Local.Source)
	assert.Equal(t, syncdomain.IssueLocalReadDenied, statuses["Private/sub/file.txt"].Local.IssueType)
	assert.Equal(t, scopeKey, statuses["Private/sub/file.txt"].Local.ScopeKey)
}

func TestFinalizeInspectorRead_PreservesSuccessfulReadOnCloseError(t *testing.T) {
	t.Parallel()

	result, err := finalizeInspectorRead("state.db", syncfixture.NewTestLogger(t), true, nil, assert.AnError)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestFinalizeInspectorRead_JoinsReadAndCloseErrors(t *testing.T) {
	t.Parallel()

	readErr := assert.AnError
	closeErr := context.Canceled

	_, err := finalizeInspectorRead("state.db", syncfixture.NewTestLogger(t), false, readErr, closeErr)
	require.Error(t, err)
	require.ErrorIs(t, err, readErr)
	require.ErrorIs(t, err, closeErr)
}

// Validates: R-2.5.2, R-2.10.8, R-6.8.24
//
// A batch release makes exactly the named rows ready and keeps the scope row
// while blocked rows remain; the batch that releases the last one deletes the
// scope row in the same transaction. observation_issues are untouched.
func TestSyncStore_ReleaseScopeWork_RemovesTheScopeWithItsLastBlockedRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")
	now := time.Date(2026, 4, 19, 9, 30, 0, 0, time.UTC)

	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(scopeKey, 5*time.Second, now)))
	seedObservationIssueForTest(t, store, "Shared/Docs/bad.txt", syncdomain.IssueRemoteWriteDenied, scopeKey)
	seedObservationIssueForTest(t, store, "other/problem.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})
	work := []syncdomain.RetryWorkKey{
		syncfixture.TestRetryWorkKey("Shared/Docs/a.txt", "", syncdomain.ActionUpload),
		syncfixture.TestRetryWorkKey("Shared/Docs/b.txt", "", syncdomain.ActionUpload),
		syncfixture.TestRetryWorkKey("Shared/Docs/c.txt", "", syncdomain.ActionUpload),
	}
	for _, key := range work {
		_, err := store.RecordBlockedRetryWork(ctx, key, scopeKey, nil)
		require.NoError(t, err)
	}

	released, removed, err := store.ReleaseScopeWork(ctx, scopeKey, work[:2], now)
	require.NoError(t, err)
	assert.False(t, removed, "a blocked row remains, so the scope stays")
	require.Len(t, released, 2, "the two named rows are returned as released")
	for _, row := range released {
		assert.False(t, row.Blocked)
		assert.Equal(t, now.UnixNano(), row.NextRetryAt)
	}

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)

	retryRows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 3)
	for _, row := range retryRows {
		if row.Path == "Shared/Docs/c.txt" {
			assert.True(t, row.Blocked)
			assert.Zero(t, row.NextRetryAt)
			continue
		}
		assert.False(t, row.Blocked, "%s was named", row.Path)
		assert.Equal(t, now.UnixNano(), row.NextRetryAt)
	}

	later := now.Add(time.Minute)
	released, removed, err = store.ReleaseScopeWork(ctx, scopeKey, append([]syncdomain.RetryWorkKey{work[0]}, work[2:]...), later)
	require.NoError(t, err)
	assert.True(t, removed, "the last blocked row released removes the scope")
	require.Len(t, released, 1, "a named row released before is not released again")
	assert.Equal(t, "Shared/Docs/c.txt", released[0].Path)

	blocks, err = store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	retryRows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 3)
	for _, row := range retryRows {
		assert.False(t, row.Blocked)
	}

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Shared/Docs/bad.txt", "other/problem.txt"}, []string{rows[0].Path, rows[1].Path})
}

// Validates: R-6.8.24
//
// A release that names nothing still removes a scope with no blocked work:
// the scope row never outlives its blocked rows.
func TestSyncStore_ReleaseScopeWork_EmptyBatchRemovesAScopeWithoutBlockedWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 9, 30, 0, 0, time.UTC)
	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(syncdomain.SKService(), 5*time.Second, now)))

	released, removed, err := store.ReleaseScopeWork(ctx, syncdomain.SKService(), nil, now)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.Empty(t, released)

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.10.33
func TestPruneBlockScopesWithoutBlockedWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(syncdomain.SKService(), time.Minute, time.Unix(160, 0))))
	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(
		syncdomain.SKThrottleDrive(driveid.New("0000000000000001")),
		time.Minute,
		time.Unix(360, 0),
	)))

	require.NoError(t, store.UpsertRetryWork(ctx, testBlockedRetryWorkRow("blocked.txt", "", syncdomain.ActionUpload, syncdomain.SKService(), 1)))

	require.NoError(t, store.PruneBlockScopesWithoutBlockedWork(ctx))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, syncdomain.SKService(), blocks[0].Key)
}

// Validates: R-2.5.2, R-2.10.8
func TestSyncStore_DiscardScope_DeletesBlockedRetryWorkAndPreservesObservationIssues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(
		scopeKey,
		time.Minute,
		time.Date(2026, 4, 19, 8, 1, 0, 0, time.UTC),
	)))
	seedObservationIssueForTest(t, store, "Shared/Docs/bad.txt", syncdomain.IssueRemoteWriteDenied, scopeKey)
	seedObservationIssueForTest(t, store, "keep.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})
	_, err := store.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("Shared/Docs/file.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)
	evidence, err := store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work:     syncfixture.TestRetryWorkKey("Shared/Docs/evidence.txt", "", syncdomain.ActionUpload),
		ScopeKey: scopeKey,
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)
	evidence, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work:     evidence.WorkKey(),
		ScopeKey: scopeKey,
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)
	require.Equal(t, 2, evidence.AttemptCount)

	deleted, err := store.DiscardScope(ctx, scopeKey)
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.RetryWorkKey{syncfixture.TestRetryWorkKey("Shared/Docs/file.txt", "", syncdomain.ActionUpload)}, deleted)

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Shared/Docs/bad.txt", "keep.txt"}, []string{rows[0].Path, rows[1].Path})

	retryRows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 1, "the unblocked row that names the scope survives")
	assert.Equal(t, "Shared/Docs/evidence.txt", retryRows[0].Path)
	assert.Equal(t, 2, retryRows[0].AttemptCount, "with its attempt count")
	assert.Equal(t, evidence.NextRetryAt, retryRows[0].NextRetryAt, "and its next retry time")
}

func TestSyncStore_DiscardScope_RejectsZeroScopeKey(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	_, err := store.DiscardScope(t.Context(), syncdomain.ScopeKey{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scope key")
}

// A discard reads only the rows blocked under its own scope: a blocked row
// whose stored scope key no longer parses, under another scope, neither
// fails the discard nor is deleted by it.
//
// Validates: R-2.5.2
func TestSyncStore_DiscardScope_IgnoresRowsOfOtherScopes(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	scopeKey := syncdomain.SKService()
	require.NoError(t, store.UpsertBlockScope(ctx, syncfixture.TestBlockScope(scopeKey, time.Minute, time.Unix(160, 0))))
	_, err := store.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("mine.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO retry_work
		(path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at)
		VALUES ('other.txt', '', ?, 'unknown:scope', 1, 1, 0)`, syncdomain.ActionUpload.String())
	require.NoError(t, err)

	deleted, err := store.DiscardScope(ctx, scopeKey)
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.RetryWorkKey{syncfixture.TestRetryWorkKey("mine.txt", "", syncdomain.ActionUpload)}, deleted)
	assert.Equal(t, 1, retryWorkRowCountForStoreScopeTest(t, store, "other.txt"), "the other scope's row stays")
}
