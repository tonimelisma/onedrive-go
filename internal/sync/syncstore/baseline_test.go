package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const updatedHash = "h2"

// commitAll is a test helper that commits outcomes one by one via CommitMutation.
func commitAll(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []syncdomain.BaselineMutation) {
	t.Helper()
	for i := range outcomes {
		require.NoError(t, mgr.CommitMutation(ctx, &outcomes[i]), "CommitMutation[%d]", i)
	}
}

// Validates: R-2.2
func TestNewSyncStore_CreatesDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := syncfixture.NewTestLogger(t)

	mgr, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	// Verify DB file exists by opening a direct connection.
	db, err := sql.Open("sqlite", SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.PingContext(t.Context()))
}

// Validates: R-6.5.1, R-2.5.2
func TestNewSyncStore_WALMode(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)

	var journalMode string

	ctx := t.Context()
	err := mgr.RawDBForTest().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "wal", journalMode)
}

// Validates: R-6.5.1, R-2.5.2
func TestSyncStore_Close_CheckpointsWAL(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := syncfixture.NewTestLogger(t)

	mgr, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	// Write some data to ensure WAL has content.
	ctx := t.Context()
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO baseline (path, item_id, parent_id, item_type,
		 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"/test.txt", "item1", "parent1", "file",
		"hash1", "hash1", 100, 100, 1700000000, 1700000000, "etag1")
	require.NoError(t, err)

	// Close should checkpoint and remove the WAL file.
	require.NoError(t, mgr.Close(t.Context()))

	// After TRUNCATE checkpoint, the WAL file should be empty or absent.
	walPath := dbPath + "-wal"
	info, statErr := os.Stat(walPath)
	if statErr == nil {
		// WAL file exists but should be empty after TRUNCATE.
		assert.Zero(t, info.Size(), "WAL file should be empty after TRUNCATE checkpoint")
	}
	// If WAL file doesn't exist at all, that's also fine.
}

// Validates: R-2.2
func TestNewSyncStore_AppliesSchema(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	var count int

	err := mgr.RawDBForTest().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('baseline', 'remote_state', 'observation_issues', 'block_scopes')",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 4, count, "canonical schema should create all core tables")
}

// Validates: R-2.2
func TestCheckpoint_DoesNotPruneRemoteMirrorRows(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Checkpoint no longer treats remote_state as a lifecycle queue, so mirror
	// rows are never pruned by database housekeeping.
	_, err := mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"old-item", "/old.txt", "file")
	require.NoError(t, err)

	// Insert a second row to ensure older/newer mirror entries both survive.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"new-item", "/new.txt", "file")
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx))

	var count int
	err = mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "checkpoint must preserve remote mirror rows")
}

// Validates: R-2.2
func TestCheckpoint_PreservesObservationIssuesAndRetryWork(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	now := time.Now()
	oldTime := now.Add(-48 * time.Hour).UnixNano()

	// Observation issues are durable truth-status inputs and are no longer
	// checkpoint-pruned by generic database housekeeping.
	_, err := mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/old-issue.txt", string(syncdomain.IssueInvalidFilename), "")
	require.NoError(t, err)

	// Insert a second issue so we prove checkpoint preserves all rows rather
	// than leaving a degenerate single-row case behind.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/new-issue.txt", string(syncdomain.IssueInvalidFilename), "")
	require.NoError(t, err)

	// retry_work is no longer timestamp-diagnostic storage and is likewise not
	// checkpoint-pruned.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO retry_work (path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"/pending-issue.txt", "", "upload", "", 0, 1, oldTime)
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx))

	var count int
	err = mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM observation_issues`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "checkpoint must preserve observation issues")

	err = mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM retry_work`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "checkpoint should not prune retry_work rows")
}

// Validates: R-2.2
func TestCheckpoint_PreservesRemoteMirrorRows(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert an old remote mirror row.
	_, err := mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/old.txt", "file")
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx))

	var count int
	err = mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "checkpoint should not prune the remote mirror")
}

// Validates: R-2.5.2, R-2.5.6, R-6.5.1
func TestSyncStore_ReopenPreservesDurableRecoveryTruth(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	mgr, err := NewSyncStore(ctx, dbPath, syncfixture.NewTestLogger(t))
	require.NoError(t, err)
	seedDurableRecoveryTruth(t, ctx, mgr, driveID, scopeKey, now)
	require.NoError(t, mgr.Close(ctx))

	reopened, err := NewSyncStore(ctx, dbPath, syncfixture.NewTestLogger(t))
	require.NoError(t, err)
	defer reopened.Close(ctx)
	assertDurableRecoveryTruth(t, ctx, reopened, scopeKey)
}

func seedDurableRecoveryTruth(
	t *testing.T,
	ctx context.Context,
	mgr *SyncStore,
	driveID driveid.ID,
	scopeKey syncdomain.ScopeKey,
	now time.Time,
) {
	t.Helper()
	require.NoError(t, mgr.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "docs/report.txt",
		DriveID:         driveID,
		ItemID:          "remote-report",
		ParentID:        "remote-parent",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "local-hash",
		RemoteHash:      "remote-hash",
		LocalSize:       12,
		LocalSizeKnown:  true,
		RemoteSize:      12,
		RemoteSizeKnown: true,
		LocalMtime:      now.UnixNano(),
		RemoteMtime:     now.UnixNano(),
		ETag:            "etag-report",
	}))
	require.NoError(t, mgr.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:             "docs/report.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             "local-hash",
		Size:             12,
		Mtime:            now.UnixNano(),
		LocalDevice:      42,
		LocalInode:       99,
		LocalHasIdentity: true,
	}}))
	require.NoError(t, mgr.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "remote-report",
		Path:     "docs/report.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "remote-hash",
		Size:     12,
		Mtime:    now.UnixNano(),
		ETag:     "etag-report",
	}}, "delta-token-after-report", driveID))
	require.NoError(t, mgr.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "invalid:name.txt",
			DriveID:   driveID,
			IssueType: syncdomain.IssueInvalidFilename,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueInvalidFilename},
	}))
	require.NoError(t, mgr.UpsertRetryWork(ctx, &syncdomain.RetryWorkRow{
		Path:         "retry-later.txt",
		ActionType:   syncdomain.ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  now.Add(time.Minute).UnixNano(),
	}))
	require.NoError(t, mgr.UpsertBlockScope(ctx, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))
	_, err := mgr.RecordBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "Shared/Docs/blocked.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey, nil)
	require.NoError(t, err)
}

func assertDurableRecoveryTruth(t *testing.T, ctx context.Context, store *SyncStore, scopeKey syncdomain.ScopeKey) {
	t.Helper()

	baseline, err := store.Load(ctx)
	require.NoError(t, err)
	entry, found := baseline.GetByPath("docs/report.txt")
	require.True(t, found)
	assert.Equal(t, "remote-report", entry.ItemID)
	assert.Equal(t, "etag-report", entry.ETag)

	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, localRows, 1)
	assert.Equal(t, "docs/report.txt", localRows[0].Path)
	assert.True(t, localRows[0].LocalHasIdentity)

	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)
	require.Len(t, remoteRows, 1)
	assert.Equal(t, "remote-report", remoteRows[0].ItemID)

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "delta-token-after-report", state.Cursor)
	assert.True(t, state.LocalTruthComplete)

	issues, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, syncdomain.IssueInvalidFilename, issues[0].IssueType)

	retryRows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 2)

	blockedRows, err := store.ListBlockedRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, blockedRows, 1)
	assert.Equal(t, scopeKey, blockedRows[0].ScopeKey)

	blockScopes, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, scopeKey, blockScopes[0].Key)
}

// Validates: R-2.2
func TestLoad_EmptyBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	b, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, b.Len())
	_, idOk := b.GetByID("nonexistent")
	assert.False(t, idOk)
}

// Validates: R-2.2.4, R-6.2.4
//
// A content change the verified local delete proved is recorded by clearing
// the row's stored change time and nothing else, in the store and in the
// cached baseline alike, so the next scan reads the file instead of trusting
// the row. A path with no row is left alone.
func TestSyncStore_ForgetBaselineLocalCtime_ClearsOnlyTheChangeTime(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	synced := syncdomain.BaselineMutation{
		Action:                syncdomain.ActionDownload,
		Success:               true,
		Path:                  "docs/readme.md",
		DriveID:               driveid.New("drive1"),
		ItemID:                "item1",
		ItemType:              syncdomain.ItemTypeFile,
		LocalHash:             "abc123",
		RemoteHash:            "abc123",
		LocalSize:             1024,
		LocalSizeKnown:        true,
		LocalMtime:            1_700_000_000_000_000_000,
		LocalCtime:            1_700_000_000_500_000_000,
		LocalDevice:           7,
		LocalInode:            42,
		LocalHasIdentity:      true,
		LocalIdentityObserved: true,
	}
	commitAll(t, mgr, ctx, []syncdomain.BaselineMutation{synced})
	before, ok := mgr.cachedBaseline().GetByPath(synced.Path)
	require.True(t, ok)
	want := before
	want.LocalCtime = 0

	require.NoError(t, mgr.ForgetBaselineLocalCtime(ctx, synced.Path))
	require.NoError(t, mgr.ForgetBaselineLocalCtime(ctx, "no/such/row.txt"))

	cached, ok := mgr.cachedBaseline().GetByPath(synced.Path)
	require.True(t, ok)
	assert.Equal(t, want, cached)
	require.NoError(t, mgr.ReloadBaselineCacheForTest(ctx))
	stored, ok := mgr.cachedBaseline().GetByPath(synced.Path)
	require.True(t, ok)
	assert.Equal(t, want, stored, "the store holds the same row")
}

// Validates: R-6.5.2
func TestCommit_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []syncdomain.BaselineMutation{{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "docs/readme.md",
		DriveID:         driveid.New("drive1"),
		ItemID:          "item1",
		ParentID:        "parent1",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "abc123",
		RemoteHash:      "abc123",
		LocalSize:       1024,
		LocalSizeKnown:  true,
		RemoteSize:      1024,
		RemoteSizeKnown: true,
		LocalMtime:      fixedTime.Add(-time.Hour).UnixNano(),
		RemoteMtime:     fixedTime.Add(-time.Hour).UnixNano(),
		ETag:            "etag1",
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.cachedBaseline().GetByPath("docs/readme.md")
	require.True(t, ok, "baseline entry not found for docs/readme.md")
	assert.True(t, entry.DriveID.Equal(driveid.New("drive1")), "DriveID mismatch")
	assert.Equal(t, "item1", entry.ItemID)
	assert.Equal(t, "abc123", entry.LocalHash)
}

// Validates: R-6.5.2
func TestCommit_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []syncdomain.BaselineMutation{{
		Action:          syncdomain.ActionUpload,
		Success:         true,
		Path:            "photos/cat.jpg",
		DriveID:         driveid.New("drive2"),
		ItemID:          "item2",
		ParentID:        "parent2",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "hash-local",
		RemoteHash:      "hash-remote",
		LocalSize:       2048,
		LocalSizeKnown:  true,
		RemoteSize:      2048,
		RemoteSizeKnown: true,
		LocalMtime:      fixedTime.UnixNano(),
		RemoteMtime:     fixedTime.UnixNano(),
		ETag:            "etag2",
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.cachedBaseline().GetByPath("photos/cat.jpg")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "hash-local", entry.LocalHash)
	assert.Equal(t, "hash-remote", entry.RemoteHash)
}

// Validates: R-2.2
func TestCommit_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time {
		return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	})

	outcomes := []syncdomain.BaselineMutation{{
		Action:   syncdomain.ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("drive1"),
		ItemID:   "folder1",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFolder,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.cachedBaseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, syncdomain.ItemTypeFolder, entry.ItemType)
	// Folders have no hash or size.
	assert.Empty(t, entry.LocalHash)
	assert.Zero(t, entry.LocalSize)
	assert.False(t, entry.LocalSizeKnown)
	assert.Zero(t, entry.RemoteSize)
	assert.False(t, entry.RemoteSizeKnown)
}

// Validates: R-2.2
func TestCommit_BaselineUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// First commit: create baseline entry.
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return t1 })

	outcomes := []syncdomain.BaselineMutation{{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "h1",
		RemoteHash:      "h1",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
		LocalMtime:      t1.UnixNano(),
		RemoteMtime:     t1.UnixNano(),
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Second commit: convergent edit updates the stored baseline tuple.
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return t2 })

	outcomes[0].Action = syncdomain.ActionBaselineUpdate
	outcomes[0].LocalHash = updatedHash
	outcomes[0].RemoteHash = updatedHash

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.cachedBaseline().GetByPath("file.txt")
	require.True(t, ok)
	assert.Equal(t, updatedHash, entry.LocalHash)
}

// Validates: R-6.5.2, R-2.2
func TestCommit_DeleteLikeActionsRemoveBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		path         string
		deleteAction syncdomain.ActionType
	}{
		{name: "LocalDelete", path: "delete-me.txt", deleteAction: syncdomain.ActionLocalDelete},
		{name: "RemoteDelete", path: "remote-del.txt", deleteAction: syncdomain.ActionRemoteDelete},
		{name: "Cleanup", path: "cleanup.txt", deleteAction: syncdomain.ActionCleanup},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			mgr.SetNowFunc(func() time.Time {
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			})

			create := []syncdomain.BaselineMutation{{
				Action: syncdomain.ActionDownload, Success: true,
				Path: tc.path, DriveID: driveid.New("d"), ItemID: "i",
				ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
				LocalSize: 50, LocalSizeKnown: true,
				RemoteSize: 50, RemoteSizeKnown: true,
				LocalMtime: 1, RemoteMtime: 1,
			}}
			commitAll(t, mgr, ctx, create)

			remove := []syncdomain.BaselineMutation{{
				Action: tc.deleteAction, Success: true, Path: tc.path,
			}}
			commitAll(t, mgr, ctx, remove)

			_, ok := mgr.cachedBaseline().GetByPath(tc.path)
			assert.False(t, ok, "entry still exists after %s", tc.name)
		})
	}
}

// Validates: R-2.2
func TestCommit_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	// Create original entry.
	create := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "old/path.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	// Move to new path.
	move := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionLocalMove, Success: true,
		Path: "new/path.txt", OldPath: "old/path.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, move)

	_, ok := mgr.cachedBaseline().GetByPath("old/path.txt")
	assert.False(t, ok, "old path still exists after move")

	entry, ok := mgr.cachedBaseline().GetByPath("new/path.txt")
	require.True(t, ok, "new path not found after move")
	assert.Equal(t, "i", entry.ItemID)
}

// Validates: R-6.5.2
func TestCommit_SkipsFailedOutcomes(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	outcomes := []syncdomain.BaselineMutation{{
		Action:  syncdomain.ActionDownload,
		Success: false, // should be skipped
		Path:    "should-not-exist.txt",
		DriveID: driveid.New("d"), ItemID: "i", ItemType: syncdomain.ItemTypeFile,
		LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
	}}

	commitAll(t, mgr, ctx, outcomes)

	b, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, b.Len())
}

// Validates: R-2.2
func TestCommit_DeltaTokenRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		driveID    string
		tokenSteps []string
	}{
		{name: "Commit", driveID: "d", tokenSteps: []string{"token-abc"}},
		{name: "CommitUpdate", driveID: "d", tokenSteps: []string{"token-1", "token-2"}},
		{name: "GetAfterCommit", driveID: "mydrv", tokenSteps: []string{"saved-token"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			mgr.SetNowFunc(func() time.Time {
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			})

			outcomes := []syncdomain.BaselineMutation{{
				Action: syncdomain.ActionDownload, Success: true,
				Path: "f.txt", DriveID: driveid.New(tc.driveID), ItemID: "i", ItemType: syncdomain.ItemTypeFile,
				LocalHash: "h", RemoteHash: "h",
				LocalSize: 10, LocalSizeKnown: true,
				RemoteSize: 10, RemoteSizeKnown: true,
				LocalMtime: 1, RemoteMtime: 1,
			}}

			for step, token := range tc.tokenSteps {
				commitAll(t, mgr, ctx, outcomes)
				saveObservationCursorForTest(t, mgr, ctx, tc.driveID, token)
				outcomes[0].LocalHash = fmt.Sprintf("h-%d", step+2)
				outcomes[0].RemoteHash = outcomes[0].LocalHash
			}

			savedToken := readObservationCursorForTest(t, mgr, ctx, tc.driveID)
			assert.Equal(t, tc.tokenSteps[len(tc.tokenSteps)-1], savedToken)
		})
	}
}

// Validates: R-2.2
func TestCommit_RefreshesCache(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	// Verify baseline is nil before first commit.
	assert.Nil(t, mgr.cachedBaseline(), "baseline should be nil before first Load/Commit")

	outcomes := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: syncdomain.ItemTypeFile,
		LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	require.NotNil(t, mgr.cachedBaseline(), "baseline should be populated after Commit")
	assert.Equal(t, 1, mgr.cachedBaseline().Len())
}

// Validates: R-2.2
func TestReadObservationState_EmptyCursor(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Empty(t, state.Cursor)
}

// Validates: R-2.2
func TestMarkFullRemoteRefresh_PersistsNextRefreshAt(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	at := time.Unix(20_000, 0)

	require.NoError(t, mgr.MarkFullRemoteRefresh(ctx, driveid.New("drive-1"), at, syncdomain.RemoteObservationModeDelta))

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, at.Add(24*time.Hour).UnixNano(), state.NextFullRemoteRefreshAt)
}

// Validates: R-2.2
func TestLoad_NullableFields(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert a row with NULL parent_id, hashes, size, mtimes, etag directly.
	_, err := mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO baseline (path, item_id, parent_id, item_type,
		 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		 VALUES (?, ?, NULL, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`,
		"root", "root-id", "root",
	)
	require.NoError(t, err)

	b, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := b.GetByPath("root")
	require.True(t, ok, "root entry not found")
	assert.Empty(t, entry.ParentID)
	assert.Empty(t, entry.LocalHash)
	assert.Empty(t, entry.RemoteHash)
	assert.Zero(t, entry.LocalSize)
	assert.False(t, entry.LocalSizeKnown)
	assert.Zero(t, entry.RemoteSize)
	assert.False(t, entry.RemoteSizeKnown)
	assert.Zero(t, entry.LocalMtime)
	assert.Zero(t, entry.RemoteMtime)
	assert.Empty(t, entry.ETag)
}

// ---------------------------------------------------------------------------
// Baseline cache tests
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestLoad_ReturnsCachedBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed a baseline entry.
	outcomes := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: syncdomain.ItemTypeFile,
		LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	// First Load returns the cached baseline from Commit's refresh.
	b1, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Second Load should return the same pointer (cached, no DB query).
	b2, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Same(t, b1, b2, "second Load returned a different *Baseline; expected cached pointer")
	assert.Equal(t, 1, b2.Len())
}

// Validates: R-2.2
func TestLoad_CacheInvalidatedByCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed one entry.
	outcomes := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "first.txt", DriveID: driveid.New("d"), ItemID: "i1", ItemType: syncdomain.ItemTypeFile,
		LocalHash: "h1", RemoteHash: "h1",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	b1, err := mgr.Load(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, b1.Len())

	// Commit a second entry — cache should be invalidated and refreshed.
	outcomes2 := []syncdomain.BaselineMutation{{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "second.txt", DriveID: driveid.New("d"), ItemID: "i2", ItemType: syncdomain.ItemTypeFile,
		LocalHash: updatedHash, RemoteHash: updatedHash,
		LocalSize: 20, LocalSizeKnown: true,
		RemoteSize: 20, RemoteSizeKnown: true,
		LocalMtime: 2, RemoteMtime: 2,
	}}

	commitAll(t, mgr, ctx, outcomes2)

	b2, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, b2.Len(), "cache should reflect both commits")
}

// Validates: R-2.2
func TestSchemaBootstrap_Idempotent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := syncfixture.NewTestLogger(t)

	// First open: applies canonical schema.
	mgr1, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	require.NoError(t, mgr1.Close(t.Context()))

	// Second open: schema bootstrap should be a no-op.
	mgr2, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr2.Close(t.Context())

	// Verify the DB is still functional.
	ctx := t.Context()

	b, err := mgr2.Load(ctx)
	require.NoError(t, err)
	assert.NotNil(t, b)
}

// ---------------------------------------------------------------------------
// CommitMutation tests
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestCommitMutation_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "co-download.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i1",
		ParentID:        "p1",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "lh",
		RemoteHash:      "rh",
		LocalSize:       512,
		LocalSizeKnown:  true,
		RemoteSize:      512,
		RemoteSizeKnown: true,
		LocalMtime:      fixedTime.UnixNano(),
		RemoteMtime:     fixedTime.UnixNano(),
		ETag:            "etag1",
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	entry, ok := mgr.cachedBaseline().GetByPath("co-download.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i1", entry.ItemID)
	assert.Equal(t, "lh", entry.LocalHash)
}

// Validates: R-2.2
func TestCommitMutation_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionUpload,
		Success:         true,
		Path:            "co-upload.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i2",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "h",
		RemoteHash:      "h",
		LocalSize:       256,
		LocalSizeKnown:  true,
		RemoteSize:      256,
		RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	entry, ok := mgr.cachedBaseline().GetByPath("co-upload.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i2", entry.ItemID)
}

// Validates: R-2.5.7
func TestCommitMutation_EmptyItemIDFails(t *testing.T) {
	t.Parallel()

	for _, action := range []syncdomain.ActionType{syncdomain.ActionUpload, syncdomain.ActionDownload, syncdomain.ActionFolderCreate, syncdomain.ActionRemoteMove} {
		t.Run(action.String(), func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			outcome := syncdomain.BaselineMutation{
				Action:   action,
				Success:  true,
				Path:     "no-id.txt",
				OldPath:  "old.txt",
				DriveID:  driveid.New("d"),
				ItemType: syncdomain.ItemTypeFile,
			}

			// An empty item_id is the baseline's primary key: the next such
			// outcome would rewrite this row's path.
			err := mgr.CommitMutation(t.Context(), &outcome)
			require.ErrorIs(t, err, errMutationWithoutItemID)

			_, found := mgr.cachedBaseline().GetByPath("no-id.txt")
			assert.False(t, found)
		})
	}
}

// Validates: R-6.7.17
func TestCommitMutation_PersistsSideAwareFileMetadata(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC) })

	outcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionUpload,
		Success:         true,
		Path:            "hashless.docx",
		DriveID:         driveid.New("d"),
		ItemID:          "i-side-aware",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "",
		RemoteHash:      "",
		LocalSize:       512,
		LocalSizeKnown:  true,
		RemoteSize:      768,
		RemoteSizeKnown: true,
		LocalMtime:      111,
		RemoteMtime:     222,
		ETag:            "etag-side-aware",
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	entry, ok := mgr.cachedBaseline().GetByPath("hashless.docx")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, int64(512), entry.LocalSize)
	assert.True(t, entry.LocalSizeKnown)
	assert.Equal(t, int64(768), entry.RemoteSize)
	assert.True(t, entry.RemoteSizeKnown)
	assert.Equal(t, int64(111), entry.LocalMtime)
	assert.Equal(t, int64(222), entry.RemoteMtime)
	assert.Equal(t, "etag-side-aware", entry.ETag)
}

// Validates: R-6.7.17
func TestCommitMutation_PersistsZeroByteSizeAsKnownZero(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	outcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "zero.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i-zero",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "",
		RemoteHash:      "",
		LocalSize:       0,
		LocalSizeKnown:  true,
		RemoteSize:      0,
		RemoteSizeKnown: true,
		LocalMtime:      100,
		RemoteMtime:     100,
		ETag:            "etag-zero",
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	var (
		localSize       sql.NullInt64
		remoteSize      sql.NullInt64
		storedLocalETag sql.NullString
	)

	err := mgr.RawDBForTest().QueryRowContext(ctx,
		`SELECT local_size, remote_size, etag FROM baseline WHERE path = ?`,
		"zero.txt",
	).Scan(&localSize, &remoteSize, &storedLocalETag)
	require.NoError(t, err)
	require.True(t, localSize.Valid, "local_size should stay known for zero-byte files")
	require.True(t, remoteSize.Valid, "remote_size should stay known for zero-byte files")
	assert.Equal(t, int64(0), localSize.Int64)
	assert.Equal(t, int64(0), remoteSize.Int64)
	assert.Equal(t, "etag-zero", storedLocalETag.String)
}

// Validates: R-2.5.6
func TestNewSyncStore_RejectsUnversionedExistingStateDB(t *testing.T) {
	t.Parallel()

	requireNewSyncStoreRefusesSchema(t, `
		CREATE TABLE baseline (
			drive_id TEXT NOT NULL,
			item_id TEXT NOT NULL,
			path TEXT NOT NULL UNIQUE,
			parent_id TEXT,
			item_type TEXT NOT NULL,
			local_hash TEXT,
			remote_hash TEXT,
			size INTEGER,
			mtime INTEGER,
			synced_at INTEGER NOT NULL,
			etag TEXT,
			PRIMARY KEY (drive_id, item_id)
		);
		INSERT INTO baseline (
			drive_id, item_id, path, parent_id, item_type,
			local_hash, remote_hash, size, mtime, synced_at, etag
		) VALUES (
			'drive1', 'item1', 'legacy.txt', 'parent1', 'file',
			'', '', 123, 456, 789, 'etag-legacy'
		);
	`)
}

// Validates: R-2.2
func TestCommitMutation_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed an entry first.
	seed := syncdomain.BaselineMutation{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "co-delete.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	del := syncdomain.BaselineMutation{Action: syncdomain.ActionLocalDelete, Success: true, Path: "co-delete.txt"}
	require.NoError(t, mgr.CommitMutation(ctx, &del))

	_, ok := mgr.cachedBaseline().GetByPath("co-delete.txt")
	assert.False(t, ok, "entry still exists after delete")
}

// Validates: R-2.17.13
//
// The delete of an empty folder made here, which a synced folder's move takes
// the place of, names no synced item: its commit leaves the row at its path,
// the item that lands there, as it is.
func TestCommitMutation_DeleteOfAnUnsyncedEntryRecordsNothing(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	seed := syncdomain.BaselineMutation{
		Action: syncdomain.ActionFolderCreate, Success: true,
		Path: "p", DriveID: driveid.New("d"), ItemID: "item-p", ItemType: syncdomain.ItemTypeFolder,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seed))
	_, seeded := mgr.cachedBaseline().GetByPath("p")
	require.True(t, seeded)

	del := syncdomain.BaselineMutation{
		Action: syncdomain.ActionLocalDelete, Success: true, Path: "p", ItemType: syncdomain.ItemTypeFolder,
		DeletesUnsyncedEntry: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &del))

	entry, ok := mgr.cachedBaseline().GetByPath("p")
	require.True(t, ok, "the synced folder's row stays")
	assert.Equal(t, "item-p", entry.ItemID)
}

// Validates: R-2.2
func TestCommitMutation_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed original entry.
	seed := syncdomain.BaselineMutation{
		Action: syncdomain.ActionDownload, Success: true,
		Path: "old/move.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	move := syncdomain.BaselineMutation{
		Action: syncdomain.ActionLocalMove, Success: true,
		Path: "new/move.txt", OldPath: "old/move.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &move))

	_, ok := mgr.cachedBaseline().GetByPath("old/move.txt")
	assert.False(t, ok, "old path still exists after move")

	entry, ok := mgr.cachedBaseline().GetByPath("new/move.txt")
	require.True(t, ok, "new path not found after move")
	assert.Equal(t, "p2", entry.ParentID)
}

// Validates: R-2.17.2
func TestCommitMutation_FolderMoveRepathsSubtree(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New("d")

	for _, seed := range []syncdomain.BaselineMutation{
		{Action: syncdomain.ActionFolderCreate, Success: true, Path: "ü src", DriveID: driveID, ItemID: "dir", ItemType: syncdomain.ItemTypeFolder},
		{Action: syncdomain.ActionFolderCreate, Success: true, Path: "ü src/inner", DriveID: driveID, ItemID: "inner", ItemType: syncdomain.ItemTypeFolder},
		{
			Action: syncdomain.ActionUpload, Success: true, Path: "ü src/inner/a.txt", DriveID: driveID, ItemID: "a",
			ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		},
		// A sibling sharing the name prefix must not move.
		{Action: syncdomain.ActionFolderCreate, Success: true, Path: "ü src2", DriveID: driveID, ItemID: "sibling", ItemType: syncdomain.ItemTypeFolder},
	} {
		require.NoError(t, mgr.CommitMutation(ctx, &seed))
	}

	move := syncdomain.BaselineMutation{
		Action: syncdomain.ActionRemoteMove, Success: true, Path: "日本 dst", OldPath: "ü src",
		DriveID: driveID, ItemID: "dir", ItemType: syncdomain.ItemTypeFolder,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &move))

	for _, cached := range []bool{true, false} {
		var bl syncdomain.BaselineView = mgr.cachedBaseline()
		if !cached {
			var err error
			bl, err = mgr.Load(ctx)
			require.NoError(t, err)
		}
		for _, p := range []string{"日本 dst", "日本 dst/inner", "日本 dst/inner/a.txt", "ü src2"} {
			_, ok := bl.GetByPath(p)
			assert.True(t, ok, "missing %s (cached=%v)", p, cached)
		}
		for _, p := range []string{"ü src", "ü src/inner", "ü src/inner/a.txt"} {
			_, ok := bl.GetByPath(p)
			assert.False(t, ok, "stale %s (cached=%v)", p, cached)
		}
	}
}

// Validates: R-2.2
func TestCommitMutation_SkipsFailedOutcome(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	outcome := syncdomain.BaselineMutation{
		Action:  syncdomain.ActionDownload,
		Success: false,
		Path:    "should-not-exist.txt",
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	if mgr.cachedBaseline() != nil {
		_, ok := mgr.cachedBaseline().GetByPath("should-not-exist.txt")
		assert.False(t, ok, "failed outcome should not create baseline entry")
	}
}

// Validates: R-2.2
func TestCommitMutation_UnknownAction_ReturnsErrorAndSkipsDBWrite(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	err := mgr.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionType(999),
		Success:  true,
		Path:     "unknown.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "unknown-item",
		ItemType: syncdomain.ItemTypeFile,
	})
	require.ErrorContains(t, err, syncdomain.OutsideTheEnum(syncdomain.ActionType(999)))

	var count int
	queryErr := mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline WHERE path = ?`, "unknown.txt").Scan(&count)
	require.NoError(t, queryErr)
	assert.Zero(t, count, "unknown actions must not write baseline rows")

	_, ok := mgr.cachedBaseline().GetByPath("unknown.txt")
	assert.False(t, ok, "unknown actions must not mutate the in-memory cache")
}

// Validates: R-2.2, R-6.5.2
//
// The cache patch is decided inside the outcome's transaction, so an
// outcome the store cannot classify fails before COMMIT and touches
// neither the database nor the cache.
func TestCommitMutation_UnknownActionChangesNeitherDatabaseNorCache(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	seed := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "cached.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "item-cached",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       "hash1",
		RemoteHash:      "hash1",
		LocalSize:       42,
		LocalSizeKnown:  true,
		RemoteSize:      42,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	err := mgr.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action: syncdomain.ActionType(999), Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "item-other",
	})
	require.Error(t, err)

	entry, ok := mgr.cachedBaseline().GetByPath("cached.txt")
	require.True(t, ok)
	assert.Equal(t, "item-cached", entry.ItemID)
	require.NoError(t, mgr.CheckCacheConsistency(ctx))
}

// Validates: R-2.2
func TestCommitMutation_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := syncdomain.BaselineMutation{
		Action:   syncdomain.ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("d"),
		ItemID:   "folder-id",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFolder,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	entry, ok := mgr.cachedBaseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, syncdomain.ItemTypeFolder, entry.ItemType)
	assert.Equal(t, "folder-id", entry.ItemID)
}

// Validates: R-2.2
// TestCommitMutation_Upload_NewItemID_SamePath verifies that when an upload
// outcome has a different item_id than the existing baseline entry at the same
// path (e.g., server-side delete+recreate assigns new ID), the stale row is
// replaced and no UNIQUE constraint violation occurs.
func TestCommitMutation_Upload_NewItemID_SamePath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	driveID := driveid.New("d1")

	// Seed baseline with item_id "old-id" at path "file.txt".
	seedOutcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "old-id",
		ItemType:        syncdomain.ItemTypeFile,
		RemoteHash:      "hash1",
		LocalHash:       "hash1",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seedOutcome))

	// Upload outcome with different item_id for the same path.
	uploadOutcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionUpload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "new-id",
		ItemType:        syncdomain.ItemTypeFile,
		RemoteHash:      "hash2",
		LocalHash:       "hash2",
		LocalSize:       200,
		LocalSizeKnown:  true,
		RemoteSize:      200,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &uploadOutcome))

	// Verify the entry now has the new item_id.
	entry, ok := mgr.cachedBaseline().GetByPath("file.txt")
	require.True(t, ok, "entry should exist")
	assert.Equal(t, "new-id", entry.ItemID)
	assert.Equal(t, "hash2", entry.RemoteHash)

	// Old ID should no longer exist in ByID.
	_, ok = mgr.cachedBaseline().GetByID("old-id")
	assert.False(t, ok, "old ID should be removed from ByID")

	// New ID should exist in ByID.
	_, ok = mgr.cachedBaseline().GetByID("new-id")
	assert.True(t, ok, "new ID should exist in ByID")
}

// ---------------------------------------------------------------------------
// Observation cursor tests
// ---------------------------------------------------------------------------

// Validates: R-2.15.1
func TestCommitObservationCursor(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d"), "token-abc"))

	token := readObservationCursorForTest(t, mgr, ctx, "d")
	assert.Equal(t, "token-abc", token)
}

// Validates: R-2.15.1
func TestCommitObservationCursor_Update(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d"), "token-1"))
	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d"), "token-2"))

	token := readObservationCursorForTest(t, mgr, ctx, "d")
	assert.Equal(t, "token-2", token)
}

// Validates: R-2.15.1
func TestClearObservationCursor(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d"), "token-1"))
	require.NoError(t, mgr.ClearObservationCursor(ctx))

	token := readObservationCursorForTest(t, mgr, ctx, "d")
	assert.Empty(t, token)
}

// ---------------------------------------------------------------------------
// Baseline cache tests (GetByPath, GetByID, put, delete, Len, ForEachPath)
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestBaseline_GetByPath(t *testing.T) {
	t.Parallel()

	b := cacheOf(&syncdomain.BaselineEntry{Path: "docs/readme.md", ItemID: "item1", DriveID: driveid.New("d1")})

	entry, ok := b.GetByPath("docs/readme.md")
	require.True(t, ok)
	assert.Equal(t, "item1", entry.ItemID)

	_, ok = b.GetByPath("nonexistent")
	assert.False(t, ok)
}

// Validates: R-2.2
func TestBaseline_GetByID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	entry := &syncdomain.BaselineEntry{Path: "test.txt", ItemID: "item1", DriveID: driveID}

	b := cacheOf(entry)

	got, ok := b.GetByID("item1")
	require.True(t, ok)
	assert.Equal(t, "test.txt", got.Path)

	_, ok = b.GetByID("nonexistent")
	assert.False(t, ok)
}

// Validates: R-2.2
func TestBaseline_Put(t *testing.T) {
	t.Parallel()

	b := cacheOf()

	entry := &syncdomain.BaselineEntry{
		Path:    "new/file.txt",
		DriveID: driveid.New("d1"),
		ItemID:  "item-new",
	}

	b.put(entry)

	got, ok := b.GetByPath("new/file.txt")
	require.True(t, ok, "entry not found after Put")
	assert.Equal(t, "item-new", got.ItemID)

	gotByID, ok := b.GetByID("item-new")
	require.True(t, ok, "entry not found by ID after Put")
	assert.Equal(t, "new/file.txt", gotByID.Path)
}

// TestBaseline_Put_ReplacesStaleID verifies that Put at an existing path with
// a different (driveID, itemID) removes the stale ByID entry.
// Validates: R-2.2
func TestBaseline_Put_ReplacesStaleID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	oldEntry := &syncdomain.BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "old-id"}

	b := cacheOf(oldEntry)

	// Put a new entry at the same path but different item_id.
	newEntry := &syncdomain.BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "new-id"}
	b.put(newEntry)

	// New entry should be accessible by path and new ID.
	got, ok := b.GetByPath("file.txt")
	require.True(t, ok, "entry not found by path")
	assert.Equal(t, "new-id", got.ItemID)

	_, ok = b.GetByID("new-id")
	assert.True(t, ok, "new ID not found in ByID")

	// Old ID should be gone.
	_, ok = b.GetByID("old-id")
	assert.False(t, ok, "stale old ID should be removed from ByID")

	// Only 1 entry total.
	assert.Equal(t, 1, b.Len())
}

// Validates: R-2.2
func TestBaseline_Delete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	entry := &syncdomain.BaselineEntry{Path: "delete-me.txt", DriveID: driveID, ItemID: "item-del"}

	b := cacheOf(entry)

	b.delete("delete-me.txt")

	_, ok := b.GetByPath("delete-me.txt")
	assert.False(t, ok, "entry still exists after Delete")

	_, ok = b.GetByID("item-del")
	assert.False(t, ok, "entry still exists in ByID after Delete")

	// Deleting nonexistent path should not panic.
	b.delete("nonexistent")
}

// Validates: R-2.2
func TestBaseline_Len(t *testing.T) {
	t.Parallel()

	b := cacheOf(&syncdomain.BaselineEntry{Path: "a.txt", ItemID: "i1"}, &syncdomain.BaselineEntry{Path: "b.txt", ItemID: "i2"})

	assert.Equal(t, 2, b.Len())
}

// Validates: R-2.2
func TestBaseline_ForEachPath(t *testing.T) {
	t.Parallel()

	b := cacheOf(&syncdomain.BaselineEntry{Path: "a.txt", ItemID: "i1"}, &syncdomain.BaselineEntry{Path: "b.txt", ItemID: "i2"})

	paths := make(map[string]bool)
	b.ForEachPath(func(path string, entry syncdomain.BaselineEntry) {
		paths[path] = true
	})

	assert.Len(t, paths, 2)
	assert.True(t, paths["a.txt"], "ForEachPath did not visit a.txt")
	assert.True(t, paths["b.txt"], "ForEachPath did not visit b.txt")
}

// Validates: R-2.2, R-6.4
func TestBaseline_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := cacheOf()

	// Seed some entries.
	for i := range 100 {
		entry := &syncdomain.BaselineEntry{
			Path:    fmt.Sprintf("file%03d.txt", i),
			DriveID: driveid.New("d1"),
			ItemID:  fmt.Sprintf("item-%03d", i),
		}
		b.put(entry)
	}

	var wg stdsync.WaitGroup

	// Concurrent readers.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				b.GetByPath(fmt.Sprintf("file%03d.txt", j))
				b.Len()
			}
		}()
	}

	// Concurrent writers.
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := &syncdomain.BaselineEntry{
				Path:    fmt.Sprintf("concurrent%d.txt", i),
				DriveID: driveid.New("d1"),
				ItemID:  fmt.Sprintf("concurrent-item-%d", i),
			}
			b.put(entry)
		}()
	}

	// Concurrent ForEachPath.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ForEachPath(func(_ string, _ syncdomain.BaselineEntry) {})
		}()
	}

	wg.Wait()

	// Baseline should have at least original + concurrent entries.
	assert.GreaterOrEqual(t, b.Len(), 100)
}

// Validates: R-2.2, R-6.5.2
//
// CheckCacheConsistency compares every field of every entry: a row changed
// behind the cache's back in any one column, or present on one side only,
// is reported, naming the path.
func TestCheckCacheConsistency_ReportsAMismatchInAnyColumn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		column, sql string
	}{
		{"local_hash", `UPDATE baseline SET local_hash = 'tampered' WHERE path = 'consistency-check.txt'`},
		{"parent_id", `UPDATE baseline SET parent_id = 'other-parent' WHERE path = 'consistency-check.txt'`},
		{"item_type", `UPDATE baseline SET item_type = 'folder' WHERE path = 'consistency-check.txt'`},
		{"etag", `UPDATE baseline SET etag = 'other-etag' WHERE path = 'consistency-check.txt'`},
		{"row only in the database", `INSERT INTO baseline (item_id, path, item_type) VALUES ('item-db', 'db-only.txt', 'file')`},
		{"row only in the cache", `DELETE FROM baseline WHERE path = 'consistency-check.txt'`},
	} {
		t.Run(tc.column, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			require.NoError(t, mgr.CommitMutation(ctx, &syncdomain.BaselineMutation{
				Action: syncdomain.ActionDownload, Success: true,
				Path: "consistency-check.txt", DriveID: driveid.New(syncfixture.TestDriveID),
				ItemID: "item-cc", ParentID: "root", ItemType: syncdomain.ItemTypeFile,
				LocalHash: "hash1", RemoteHash: "hash1",
				LocalSize: 100, LocalSizeKnown: true,
				RemoteSize: 100, RemoteSizeKnown: true,
				LocalMtime: 1000, RemoteMtime: 1000, ETag: "etag-1",
			}))
			require.NoError(t, mgr.CheckCacheConsistency(ctx))

			_, err := mgr.RawDBForTest().ExecContext(ctx, tc.sql)
			require.NoError(t, err)

			err = mgr.CheckCacheConsistency(ctx)
			require.ErrorIs(t, err, ErrBaselineCacheDiverged)
			require.ErrorContains(t, err, ".txt")
		})
	}
}

// ---------------------------------------------------------------------------
// Migration tests
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestConsolidatedSchema_AllTablesCreated(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Verify all expected tables exist by querying sqlite_master.
	expectedTables := []string{
		"baseline", "local_state", "observation_state",
		"observation_issues", "retry_work", "remote_state", "block_scopes",
		"shortcut_roots",
	}

	for _, table := range expectedTables {
		var name string
		err := mgr.RawDBForTest().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		require.NoError(t, err, "table %q should exist", table)
		assert.Equal(t, table, name)
	}

	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d!abc123"), "primary-token"))

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "primary-token", state.Cursor)

	// Verify remote_state table structure: insert + query.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/test.txt", "file")
	require.NoError(t, err)

	// Verify observation_issues table structure: insert + query.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/bad-file.txt", "invalid_filename", "")
	require.NoError(t, err)

	// Verify remote_state CHECK constraint rejects invalid item types.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item2", "/bad.txt", "bogus")
	require.Error(t, err, "invalid item type should be rejected by CHECK constraint")
}

// Validates: R-2.2
func TestConsolidatedSchema_RemoteStateActivePathUnique(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert an active item at a path.
	_, err := mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/test.txt", "file")
	require.NoError(t, err)

	// Another item at the same path should be rejected by the mirror's unique path index.
	_, err = mgr.RawDBForTest().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item2", "/test.txt", "file")
	require.Error(t, err, "duplicate active path should be rejected")
}

// Validates: R-2.2
func TestBaselineEntryCount_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	var count int
	err := mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// Validates: R-2.2
func TestBaselineEntryCount_WithEntries(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Commit a download outcome to add a baseline entry.
	outcome := syncdomain.BaselineMutation{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "/test/file.txt",
		DriveID:         driveid.New("d!123"),
		ItemID:          "item-1",
		ParentID:        "parent-1",
		ItemType:        syncdomain.ItemTypeFile,
		RemoteHash:      "hash123",
		LocalSize:       1024,
		LocalSizeKnown:  true,
		RemoteSize:      1024,
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
	}
	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	var count int
	err := mgr.RawDBForTest().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// Validates: R-2.1.3
func TestCommitMutation_PersistsLocalFilesystemIdentity(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, mgr.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:           syncdomain.ActionDownload,
		Success:          true,
		Path:             "Docs/report.txt",
		DriveID:          driveid.New("d1"),
		ItemID:           "item-report",
		ItemType:         syncdomain.ItemTypeFile,
		LocalHash:        "hash-report",
		RemoteHash:       "hash-report",
		LocalSize:        42,
		LocalSizeKnown:   true,
		RemoteSize:       42,
		RemoteSizeKnown:  true,
		LocalMtime:       123,
		RemoteMtime:      456,
		LocalDevice:      700,
		LocalInode:       900,
		LocalBirthTime:   1100,
		LocalHasIdentity: true,
	}))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)
	entry, ok := bl.GetByPath("Docs/report.txt")
	require.True(t, ok)
	assert.Equal(t, uint64(700), entry.LocalDevice)
	assert.Equal(t, uint64(900), entry.LocalInode)
	assert.Equal(t, int64(1100), entry.LocalBirthTime)
	assert.True(t, entry.LocalHasIdentity)
}

// Validates: R-2.2, R-6.2.4
//
// A failed WAL checkpoint must reach the caller. Cross-process readers such as
// `status` open their own connection and rely on the checkpoint having
// happened, so silently swallowing the failure would let them read stale data
// with no signal.
func TestSyncStore_CheckpointReportsFailure(t *testing.T) {
	t.Parallel()

	mgr, err := NewSyncStore(t.Context(), filepath.Join(t.TempDir(), "test.db"), syncfixture.NewTestLogger(t))
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(t.Context()))
	require.NoError(t, mgr.Close(t.Context()))

	err = mgr.Checkpoint(t.Context())
	require.Error(t, err, "checkpointing a closed store must not report success")
	assert.Contains(t, err.Error(), "checkpoint sync store WAL")
}

// Validates: R-6.3.4
func TestBaselineCache_GetByPathCopyIsSafeAgainstConcurrentPut(t *testing.T) {
	t.Parallel()

	baseline := cacheOf(&syncdomain.BaselineEntry{Path: "Docs/Report.txt", ItemID: "id-upper", ItemType: syncdomain.ItemTypeFile})

	// An observer reads an entry on its own goroutine while a worker commits
	// an outcome that replaces it. The race detector fails this test when
	// the index's own entry is returned.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			baseline.put(&syncdomain.BaselineEntry{Path: "Docs/Report.txt", ItemID: fmt.Sprintf("id-%d", i), ItemType: syncdomain.ItemTypeFile})
		}
	}()

	for range 200 {
		if v, ok := baseline.GetByPath("Docs/Report.txt"); ok {
			assert.Equal(t, "Docs/Report.txt", v.Path)
		}
	}
	syncfixture.ReceiveWithin(t, done, "the concurrent writer to finish")

	entry, ok := baseline.GetByPath("Docs/Report.txt")
	require.True(t, ok)
	assert.Equal(t, "id-199", entry.ItemID)
	_, ok = baseline.GetByPath("Docs/missing.txt")
	assert.False(t, ok)
}

// cacheOf returns a baseline cache holding copies of entries.
func cacheOf(entries ...*syncdomain.BaselineEntry) *baselineCache {
	owned := make([]*syncdomain.BaselineEntry, len(entries))
	for i, entry := range entries {
		copied := *entry
		owned[i] = &copied
	}

	return newBaselineCache(indexBaselineEntries(owned))
}
