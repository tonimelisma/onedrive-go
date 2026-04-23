package sync

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
)

const updatedHash = "h2"

// commitAll is a test helper that commits outcomes one by one via CommitMutation.
func commitAll(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []BaselineMutation) {
	t.Helper()
	for i := range outcomes {
		require.NoError(t, mgr.CommitMutation(ctx, &outcomes[i]), "CommitMutation[%d]", i)
	}
}

// Validates: R-2.2
func TestNewSyncStore_CreatesDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := newTestLogger(t)

	mgr, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close(t.Context())

	// Verify DB file exists by opening a direct connection.
	db, err := sql.Open("sqlite", "file:"+dbPath)
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
	err := mgr.rawDB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "wal", journalMode)
}

// Validates: R-6.5.1, R-2.5.2
func TestSyncStore_Close_CheckpointsWAL(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := newTestLogger(t)

	mgr, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	// Write some data to ensure WAL has content.
	ctx := t.Context()
	_, err = mgr.rawDB().ExecContext(ctx,
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

	err := mgr.rawDB().QueryRowContext(ctx,
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

	retention := 24 * time.Hour // 1 day retention

	// Checkpoint no longer treats remote_state as a lifecycle queue, so mirror
	// rows are never pruned by age.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"old-item", "/old.txt", "file")
	require.NoError(t, err)

	// Insert a second row to ensure older/newer mirror entries both survive.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"new-item", "/new.txt", "file")
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx, retention))

	var count int
	err = mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
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
	retention := 24 * time.Hour

	// Observation issues are durable truth-status inputs and are no longer
	// checkpoint-pruned by timestamp.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/old-issue.txt", "invalid_name", "")
	require.NoError(t, err)

	// Insert a second issue so we prove checkpoint preserves all rows rather
	// than leaving a degenerate single-row case behind.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/new-issue.txt", "invalid_name", "")
	require.NoError(t, err)

	// retry_work is no longer timestamp-diagnostic storage and is likewise not
	// checkpoint-pruned.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO retry_work (path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"/pending-issue.txt", "", "upload", "", 0, 1, oldTime)
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx, retention))

	var count int
	err = mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM observation_issues`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "checkpoint must preserve observation issues")

	err = mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM retry_work`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "checkpoint should not prune retry_work rows")
}

// Validates: R-2.2
func TestCheckpoint_ZeroRetentionSkipsPruning(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert an old remote mirror row.
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/old.txt", "file")
	require.NoError(t, err)

	// Zero retention = WAL checkpoint only, no pruning.
	require.NoError(t, mgr.Checkpoint(ctx, 0))

	var count int
	err = mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "zero retention should not prune the remote mirror")
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

// Validates: R-6.5.2
func TestCommit_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr.setNowFunc(func() time.Time { return fixedTime })

	outcomes := []BaselineMutation{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "docs/readme.md",
		DriveID:         driveid.New("drive1"),
		ItemID:          "item1",
		ParentID:        "parent1",
		ItemType:        ItemTypeFile,
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
	mgr.setNowFunc(func() time.Time { return fixedTime })

	outcomes := []BaselineMutation{{
		Action:          ActionUpload,
		Success:         true,
		Path:            "photos/cat.jpg",
		DriveID:         driveid.New("drive2"),
		ItemID:          "item2",
		ParentID:        "parent2",
		ItemType:        ItemTypeFile,
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

	mgr.setNowFunc(func() time.Time {
		return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	})

	outcomes := []BaselineMutation{{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("drive1"),
		ItemID:   "folder1",
		ParentID: "root",
		ItemType: ItemTypeFolder,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.cachedBaseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, ItemTypeFolder, entry.ItemType)
	// Folders have no hash or size.
	assert.Empty(t, entry.LocalHash)
	assert.Zero(t, entry.LocalSize)
	assert.False(t, entry.LocalSizeKnown)
	assert.Zero(t, entry.RemoteSize)
	assert.False(t, entry.RemoteSizeKnown)
}

// Validates: R-2.2
func TestCommit_UpdateSynced(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// First commit: create baseline entry.
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.setNowFunc(func() time.Time { return t1 })

	outcomes := []BaselineMutation{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i",
		ItemType:        ItemTypeFile,
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
	mgr.setNowFunc(func() time.Time { return t2 })

	outcomes[0].Action = ActionUpdateSynced
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
		deleteAction ActionType
	}{
		{name: "LocalDelete", path: "delete-me.txt", deleteAction: ActionLocalDelete},
		{name: "RemoteDelete", path: "remote-del.txt", deleteAction: ActionRemoteDelete},
		{name: "Cleanup", path: "cleanup.txt", deleteAction: ActionCleanup},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			mgr.setNowFunc(func() time.Time {
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			})

			create := []BaselineMutation{{
				Action: ActionDownload, Success: true,
				Path: tc.path, DriveID: driveid.New("d"), ItemID: "i",
				ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
				LocalSize: 50, LocalSizeKnown: true,
				RemoteSize: 50, RemoteSizeKnown: true,
				LocalMtime: 1, RemoteMtime: 1,
			}}
			commitAll(t, mgr, ctx, create)

			remove := []BaselineMutation{{
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
	mgr.setNowFunc(func() time.Time { return fixedTime })

	// Create original entry.
	create := []BaselineMutation{{
		Action: ActionDownload, Success: true,
		Path: "old/path.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	// Move to new path.
	move := []BaselineMutation{{
		Action: ActionLocalMove, Success: true,
		Path: "new/path.txt", OldPath: "old/path.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
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

	mgr.setNowFunc(func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	outcomes := []BaselineMutation{{
		Action:  ActionDownload,
		Success: false, // should be skipped
		Path:    "should-not-exist.txt",
		DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
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
			mgr.setNowFunc(func() time.Time {
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			})

			outcomes := []BaselineMutation{{
				Action: ActionDownload, Success: true,
				Path: "f.txt", DriveID: driveid.New(tc.driveID), ItemID: "i", ItemType: ItemTypeFile,
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

	mgr.setNowFunc(func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	// Verify baseline is nil before first commit.
	assert.Nil(t, mgr.cachedBaseline(), "baseline should be nil before first Load/Commit")

	outcomes := []BaselineMutation{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
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

	require.NoError(t, mgr.MarkFullRemoteRefresh(ctx, driveid.New("drive-1"), at, remoteObservationModeDelta))

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
	_, err := mgr.rawDB().ExecContext(ctx,
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
// Conflict query + resolve tests
// ---------------------------------------------------------------------------

// seedConflict inserts a conflict via CommitMutation and returns its UUID.
// Validates: R-2.2
func TestLoad_ReturnsCachedBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	mgr.setNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed a baseline entry.
	outcomes := []BaselineMutation{{
		Action: ActionDownload, Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
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
	mgr.setNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed one entry.
	outcomes := []BaselineMutation{{
		Action: ActionDownload, Success: true,
		Path: "first.txt", DriveID: driveid.New("d"), ItemID: "i1", ItemType: ItemTypeFile,
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
	outcomes2 := []BaselineMutation{{
		Action: ActionDownload, Success: true,
		Path: "second.txt", DriveID: driveid.New("d"), ItemID: "i2", ItemType: ItemTypeFile,
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
	logger := newTestLogger(t)

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
	mgr.setNowFunc(func() time.Time { return fixedTime })

	outcome := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "co-download.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i1",
		ParentID:        "p1",
		ItemType:        ItemTypeFile,
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

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := BaselineMutation{
		Action:          ActionUpload,
		Success:         true,
		Path:            "co-upload.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i2",
		ItemType:        ItemTypeFile,
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

// Validates: R-6.7.17
func TestCommitMutation_PersistsSideAwareFileMetadata(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC) })

	outcome := BaselineMutation{
		Action:          ActionUpload,
		Success:         true,
		Path:            "hashless.docx",
		DriveID:         driveid.New("d"),
		ItemID:          "i-side-aware",
		ItemType:        ItemTypeFile,
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

	outcome := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "zero.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i-zero",
		ItemType:        ItemTypeFile,
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

	err := mgr.rawDB().QueryRowContext(ctx,
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

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), `
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
	require.NoError(t, err)
	require.NoError(t, db.Close())

	mgr, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.Error(t, err)
	require.Nil(t, mgr)
	require.ErrorIs(t, err, ErrIncompatibleSchema)
	assert.Contains(t, err.Error(), "current schema")
}

// Validates: R-2.2
func TestCommitMutation_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed an entry first.
	seed := BaselineMutation{
		Action: ActionDownload, Success: true,
		Path: "co-delete.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	del := BaselineMutation{Action: ActionLocalDelete, Success: true, Path: "co-delete.txt"}
	require.NoError(t, mgr.CommitMutation(ctx, &del))

	_, ok := mgr.cachedBaseline().GetByPath("co-delete.txt")
	assert.False(t, ok, "entry still exists after delete")
}

// Validates: R-2.2
func TestCommitMutation_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed original entry.
	seed := BaselineMutation{
		Action: ActionDownload, Success: true,
		Path: "old/move.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p1",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	move := BaselineMutation{
		Action: ActionLocalMove, Success: true,
		Path: "new/move.txt", OldPath: "old/move.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
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

// Validates: R-2.2
func TestCommitMutation_SkipsFailedOutcome(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	outcome := BaselineMutation{
		Action:  ActionDownload,
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

	err := mgr.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionType(999),
		Success:  true,
		Path:     "unknown.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "unknown-item",
		ItemType: ItemTypeFile,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown action type")

	var count int
	queryErr := mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline WHERE path = ?`, "unknown.txt").Scan(&count)
	require.NoError(t, queryErr)
	assert.Zero(t, count, "unknown actions must not write baseline rows")

	_, ok := mgr.cachedBaseline().GetByPath("unknown.txt")
	assert.False(t, ok, "unknown actions must not mutate the in-memory cache")
}

// Validates: R-2.2
func TestUpdateBaselineCache_UnknownActionReloadsFromDB(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	seed := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "cached.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "item-cached",
		ItemType:        ItemTypeFile,
		LocalHash:       "hash1",
		RemoteHash:      "hash1",
		LocalSize:       42,
		LocalSizeKnown:  true,
		RemoteSize:      42,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	mgr.cachedBaseline().Delete("cached.txt")
	_, ok := mgr.cachedBaseline().GetByPath("cached.txt")
	require.False(t, ok, "test setup should corrupt the cache before reload")

	err := mgr.updateBaselineCache(ctx, &BaselineMutation{
		Action: ActionType(999),
		Path:   "ignored.txt",
	})
	require.NoError(t, err)

	entry, ok := mgr.cachedBaseline().GetByPath("cached.txt")
	require.True(t, ok, "unknown cache mutations should trigger a DB reload")
	assert.Equal(t, "item-cached", entry.ItemID)
}

// Validates: R-2.2
func TestCommitMutation_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := BaselineMutation{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("d"),
		ItemID:   "folder-id",
		ParentID: "root",
		ItemType: ItemTypeFolder,
	}

	require.NoError(t, mgr.CommitMutation(ctx, &outcome))

	entry, ok := mgr.cachedBaseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, ItemTypeFolder, entry.ItemType)
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
	seedOutcome := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "old-id",
		ItemType:        ItemTypeFile,
		RemoteHash:      "hash1",
		LocalHash:       "hash1",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seedOutcome))

	// Upload outcome with different item_id for the same path.
	uploadOutcome := BaselineMutation{
		Action:          ActionUpload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "new-id",
		ItemType:        ItemTypeFile,
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

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New("d"), "token-abc"))

	token := readObservationCursorForTest(t, mgr, ctx, "d")
	assert.Equal(t, "token-abc", token)
}

// Validates: R-2.15.1
func TestCommitObservationCursor_Update(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.setNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

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
// Locked accessor tests (Baseline.GetByPath, GetByID, Put, Delete, Len, ForEachPath)
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestBaseline_GetByPath(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs/readme.md": {Path: "docs/readme.md", ItemID: "item1", DriveID: driveid.New("d1")},
		},
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

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
	entry := &BaselineEntry{Path: "test.txt", ItemID: "item1", DriveID: driveID}

	b := &Baseline{
		ByPath:     make(map[string]*BaselineEntry),
		ByID:       map[string]*BaselineEntry{"item1": entry},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	got, ok := b.GetByID("item1")
	require.True(t, ok)
	assert.Equal(t, "test.txt", got.Path)

	_, ok = b.GetByID("nonexistent")
	assert.False(t, ok)
}

// Validates: R-2.2
func TestBaseline_Put(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath:     make(map[string]*BaselineEntry),
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	entry := &BaselineEntry{
		Path:    "new/file.txt",
		DriveID: driveid.New("d1"),
		ItemID:  "item-new",
	}

	b.Put(entry)

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
	oldEntry := &BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "old-id"}

	b := &Baseline{
		ByPath:     map[string]*BaselineEntry{"file.txt": oldEntry},
		ByID:       map[string]*BaselineEntry{"old-id": oldEntry},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	// Put a new entry at the same path but different item_id.
	newEntry := &BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "new-id"}
	b.Put(newEntry)

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
	entry := &BaselineEntry{Path: "delete-me.txt", DriveID: driveID, ItemID: "item-del"}

	b := &Baseline{
		ByPath:     map[string]*BaselineEntry{"delete-me.txt": entry},
		ByID:       map[string]*BaselineEntry{"item-del": entry},
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	b.Delete("delete-me.txt")

	_, ok := b.GetByPath("delete-me.txt")
	assert.False(t, ok, "entry still exists after Delete")

	_, ok = b.GetByID("item-del")
	assert.False(t, ok, "entry still exists in ByID after Delete")

	// Deleting nonexistent path should not panic.
	b.Delete("nonexistent")
}

// Validates: R-2.2
func TestBaseline_Len(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"a.txt": {Path: "a.txt"},
			"b.txt": {Path: "b.txt"},
		},
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	assert.Equal(t, 2, b.Len())
}

// Validates: R-2.2
func TestBaseline_ForEachPath(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"a.txt": {Path: "a.txt", ItemID: "i1"},
			"b.txt": {Path: "b.txt", ItemID: "i2"},
		},
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	paths := make(map[string]bool)
	b.ForEachPath(func(path string, entry *BaselineEntry) {
		paths[path] = true
	})

	assert.Len(t, paths, 2)
	assert.True(t, paths["a.txt"], "ForEachPath did not visit a.txt")
	assert.True(t, paths["b.txt"], "ForEachPath did not visit b.txt")
}

// Validates: R-2.2, R-6.4
func TestBaseline_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath:     make(map[string]*BaselineEntry),
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	// Seed some entries.
	for i := range 100 {
		entry := &BaselineEntry{
			Path:    fmt.Sprintf("file%03d.txt", i),
			DriveID: driveid.New("d1"),
			ItemID:  fmt.Sprintf("item-%03d", i),
		}
		b.Put(entry)
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
			entry := &BaselineEntry{
				Path:    fmt.Sprintf("concurrent%d.txt", i),
				DriveID: driveid.New("d1"),
				ItemID:  fmt.Sprintf("concurrent-item-%d", i),
			}
			b.Put(entry)
		}()
	}

	// Concurrent ForEachPath.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ForEachPath(func(_ string, _ *BaselineEntry) {})
		}()
	}

	wg.Wait()

	// Baseline should have at least original + concurrent entries.
	assert.GreaterOrEqual(t, b.Len(), 100)
}

// Validates: R-2.2
// TestCheckCacheConsistency verifies that CheckCacheConsistency detects
// mismatches between the in-memory baseline cache and the database (B-198).
func TestCheckCacheConsistency(t *testing.T) {
	mgr := newTestStore(t)
	ctx := t.Context()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a baseline entry via CommitMutation.
	require.NoError(t, mgr.CommitMutation(ctx, &BaselineMutation{
		Action: ActionDownload, Success: true,
		Path: "consistency-check.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-cc", ParentID: "root", ItemType: ItemTypeFile,
		LocalHash: "hash1", RemoteHash: "hash1",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1000, RemoteMtime: 1000,
	}))

	// Cache and DB should be consistent — no mismatches.
	mismatches, err := mgr.CheckCacheConsistency(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, mismatches)

	// Manually corrupt the DB row behind the cache's back.
	_, err = mgr.rawDB().ExecContext(ctx,
		`UPDATE baseline SET local_hash = 'tampered' WHERE path = 'consistency-check.txt'`)
	require.NoError(t, err)

	// Now check again — should detect 1 mismatch.
	mismatches, err = mgr.CheckCacheConsistency(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, mismatches)
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
	}

	for _, table := range expectedTables {
		var name string
		err := mgr.rawDB().QueryRowContext(ctx,
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
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/test.txt", "file")
	require.NoError(t, err)

	// Verify observation_issues table structure: insert + query.
	_, err = mgr.rawDB().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key)
		 VALUES (?, ?, ?)`,
		"/bad-file.txt", "invalid_filename", "")
	require.NoError(t, err)

	// Verify remote_state CHECK constraint rejects invalid item types.
	_, err = mgr.rawDB().ExecContext(ctx,
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
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type)
		 VALUES (?, ?, ?)`,
		"item1", "/test.txt", "file")
	require.NoError(t, err)

	// Another item at the same path should be rejected by the mirror's unique path index.
	_, err = mgr.rawDB().ExecContext(ctx,
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
	err := mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// Validates: R-2.2
func TestBaselineEntryCount_WithEntries(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Commit a download outcome to add a baseline entry.
	outcome := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "/test/file.txt",
		DriveID:         driveid.New("d!123"),
		ItemID:          "item-1",
		ParentID:        "parent-1",
		ItemType:        ItemTypeFile,
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
	err := mgr.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
