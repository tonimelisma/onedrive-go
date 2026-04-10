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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const updatedHash = "h2"

// commitAll is a test helper that commits outcomes one by one via CommitOutcome.
func commitAll(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []synctypes.Outcome) {
	t.Helper()
	for i := range outcomes {
		require.NoError(t, mgr.CommitOutcome(ctx, &outcomes[i]), "CommitOutcome[%d]", i)
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
	err := mgr.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
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
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"/test.txt", "drv1", "item1", "parent1", "file",
		"hash1", "hash1", 100, 100, 1700000000, 1700000000, 1700000000, "etag1")
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

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('baseline', 'remote_state', 'sync_failures', 'scope_blocks')",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 4, count, "canonical schema should create all core tables")
}

// Validates: R-2.2
func TestCheckpoint_PrunesDeletedRemoteState(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	now := time.Now()
	oldTime := now.Add(-48 * time.Hour).UnixNano() // 2 days ago
	newTime := now.Add(-12 * time.Hour).UnixNano() // 12 hours ago
	retention := 24 * time.Hour                    // 1 day retention

	// Insert a deleted row older than retention (should be pruned).
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"drv1", "old-item", "/old.txt", "file", synctypes.SyncStatusDeleted, oldTime)
	require.NoError(t, err)

	// Insert a deleted row newer than retention (should survive).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"drv1", "new-item", "/new.txt", "file", synctypes.SyncStatusDeleted, newTime)
	require.NoError(t, err)

	// Insert a synced row (should never be pruned regardless of age).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"drv1", "synced-item", "/synced.txt", "file", synctypes.SyncStatusSynced, oldTime)
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx, retention))

	// Verify: old deleted row pruned, new deleted and synced rows survive.
	var count int
	err = mgr.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "old deleted should be pruned, new deleted + synced should remain")
}

// Validates: R-2.2
func TestCheckpoint_PrunesActionableSyncFailures(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	now := time.Now()
	oldTime := now.Add(-48 * time.Hour).UnixNano()
	newTime := now.Add(-12 * time.Hour).UnixNano()
	retention := 24 * time.Hour

	// Insert an actionable failure older than retention (should be pruned).
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"/old-issue.txt", "", "upload", "upload", "item", "actionable", oldTime, oldTime)
	require.NoError(t, err)

	// Insert an actionable failure newer than retention (should survive).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"/new-issue.txt", "", "upload", "upload", "item", "actionable", newTime, newTime)
	require.NoError(t, err)

	// Insert a transient failure (should never be pruned regardless of age).
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"/pending-issue.txt", "", "upload", "upload", "item", "transient", oldTime, oldTime)
	require.NoError(t, err)

	require.NoError(t, mgr.Checkpoint(ctx, retention))

	var count int
	err = mgr.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_failures`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "old actionable should be pruned, new actionable + transient should remain")
}

// Validates: R-2.2
func TestCheckpoint_ZeroRetentionSkipsPruning(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	oldTime := time.Now().Add(-48 * time.Hour).UnixNano()

	// Insert old deleted row.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"drv1", "item1", "/old.txt", "file", synctypes.SyncStatusDeleted, oldTime)
	require.NoError(t, err)

	// Zero retention = WAL checkpoint only, no pruning.
	require.NoError(t, mgr.Checkpoint(ctx, 0))

	var count int
	err = mgr.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_state`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "zero retention should not prune anything")
}

// Validates: R-2.2
func TestLoad_EmptyBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	b, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, b.Len())
	_, idOk := b.GetByID(driveid.NewItemKey(driveid.ID{}, "nonexistent"))
	assert.False(t, idOk)
}

// Validates: R-6.5.2
func TestCommit_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "docs/readme.md",
		DriveID:         driveid.New("drive1"),
		ItemID:          "item1",
		ParentID:        "parent1",
		ItemType:        synctypes.ItemTypeFile,
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

	entry, ok := mgr.Baseline().GetByPath("docs/readme.md")
	require.True(t, ok, "baseline entry not found for docs/readme.md")
	assert.True(t, entry.DriveID.Equal(driveid.New("drive1")), "DriveID mismatch")
	assert.Equal(t, "item1", entry.ItemID)
	assert.Equal(t, "abc123", entry.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
}

// Validates: R-6.5.2
func TestCommit_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionUpload,
		Success:         true,
		Path:            "photos/cat.jpg",
		DriveID:         driveid.New("drive2"),
		ItemID:          "item2",
		ParentID:        "parent2",
		ItemType:        synctypes.ItemTypeFile,
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

	entry, ok := mgr.Baseline().GetByPath("photos/cat.jpg")
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

	outcomes := []synctypes.Outcome{{
		Action:   synctypes.ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("drive1"),
		ItemID:   "folder1",
		ParentID: "root",
		ItemType: synctypes.ItemTypeFolder,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.Baseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, synctypes.ItemTypeFolder, entry.ItemType)
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
	mgr.SetNowFunc(func() time.Time { return t1 })

	outcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i",
		ItemType:        synctypes.ItemTypeFile,
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

	// Second commit: convergent edit updates synced_at.
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return t2 })

	outcomes[0].Action = synctypes.ActionUpdateSynced
	outcomes[0].LocalHash = updatedHash
	outcomes[0].RemoteHash = updatedHash

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.Baseline().GetByPath("file.txt")
	require.True(t, ok)
	assert.Equal(t, t2.UnixNano(), entry.SyncedAt)
	assert.Equal(t, updatedHash, entry.LocalHash)
}

// Validates: R-6.5.2, R-2.2
func TestCommit_DeleteLikeActionsRemoveBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		path         string
		deleteAction synctypes.ActionType
	}{
		{name: "LocalDelete", path: "delete-me.txt", deleteAction: synctypes.ActionLocalDelete},
		{name: "RemoteDelete", path: "remote-del.txt", deleteAction: synctypes.ActionRemoteDelete},
		{name: "Cleanup", path: "cleanup.txt", deleteAction: synctypes.ActionCleanup},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			mgr.SetNowFunc(func() time.Time {
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			})

			create := []synctypes.Outcome{{
				Action: synctypes.ActionDownload, Success: true,
				Path: tc.path, DriveID: driveid.New("d"), ItemID: "i",
				ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
				LocalSize: 50, LocalSizeKnown: true,
				RemoteSize: 50, RemoteSizeKnown: true,
				LocalMtime: 1, RemoteMtime: 1,
			}}
			commitAll(t, mgr, ctx, create)

			remove := []synctypes.Outcome{{
				Action: tc.deleteAction, Success: true, Path: tc.path,
			}}
			commitAll(t, mgr, ctx, remove)

			_, ok := mgr.Baseline().GetByPath(tc.path)
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
	create := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "old/path.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p",
		ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	// Move to new path.
	move := []synctypes.Outcome{{
		Action: synctypes.ActionLocalMove, Success: true,
		Path: "new/path.txt", OldPath: "old/path.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, move)

	_, ok := mgr.Baseline().GetByPath("old/path.txt")
	assert.False(t, ok, "old path still exists after move")

	entry, ok := mgr.Baseline().GetByPath("new/path.txt")
	require.True(t, ok, "new path not found after move")
	assert.Equal(t, "i", entry.ItemID)
}

// Validates: R-2.2, R-2.3.2
func TestCommit_Conflict(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "conflict.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify conflict row was inserted with a valid UUID.
	var id, conflictPath, conflictType, resolution string

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT id, path, conflict_type, resolution FROM conflicts LIMIT 1",
	).Scan(&id, &conflictPath, &conflictType, &resolution)
	require.NoError(t, err)

	_, uuidErr := uuid.Parse(id)
	require.NoError(t, uuidErr, "conflict id = %q is not a valid UUID", id)
	assert.Equal(t, "conflict.txt", conflictPath)
	assert.Equal(t, "edit_edit", conflictType)
	assert.Equal(t, "unresolved", resolution)
}

// Validates: R-2.2, R-2.3.2
func TestCommit_Conflict_StoresRemoteMtime(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	remoteMtime := int64(1700000000000000000) // non-zero nanosecond timestamp
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "mtime-test.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		LocalMtime:   1600000000000000000,
		RemoteMtime:  remoteMtime,
		ConflictType: "edit_edit",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify remote_mtime is stored as non-zero.
	var storedRemoteMtime sql.NullInt64

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT remote_mtime FROM conflicts WHERE path = ?", "mtime-test.txt",
	).Scan(&storedRemoteMtime)
	require.NoError(t, err)
	assert.True(t, storedRemoteMtime.Valid, "remote_mtime should be valid")
	assert.Equal(t, remoteMtime, storedRemoteMtime.Int64)
}

// Validates: R-6.5.2
func TestCommit_SkipsFailedOutcomes(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	outcomes := []synctypes.Outcome{{
		Action:  synctypes.ActionDownload,
		Success: false, // should be skipped
		Path:    "should-not-exist.txt",
		DriveID: driveid.New("d"), ItemID: "i", ItemType: synctypes.ItemTypeFile,
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

			outcomes := []synctypes.Outcome{{
				Action: synctypes.ActionDownload, Success: true,
				Path: "f.txt", DriveID: driveid.New(tc.driveID), ItemID: "i", ItemType: synctypes.ItemTypeFile,
				LocalHash: "h", RemoteHash: "h",
				LocalSize: 10, LocalSizeKnown: true,
				RemoteSize: 10, RemoteSizeKnown: true,
				LocalMtime: 1, RemoteMtime: 1,
			}}

			for step, token := range tc.tokenSteps {
				commitAll(t, mgr, ctx, outcomes)
				require.NoError(t, mgr.CommitDeltaToken(ctx, token, tc.driveID, "", tc.driveID))
				outcomes[0].LocalHash = fmt.Sprintf("h-%d", step+2)
				outcomes[0].RemoteHash = outcomes[0].LocalHash
			}

			savedToken, err := mgr.GetDeltaToken(ctx, tc.driveID, "")
			require.NoError(t, err)
			assert.Equal(t, tc.tokenSteps[len(tc.tokenSteps)-1], savedToken)
		})
	}
}

// Validates: R-2.2
func TestCommit_SyncedAtFromNowFunc(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Use a distinctive fixed time to verify nowFunc is used.
	fixedTime := time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: synctypes.ItemTypeFile,
		LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
		LocalMtime: 999, RemoteMtime: 999,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry, ok := mgr.Baseline().GetByPath("f.txt")
	require.True(t, ok)
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
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
	assert.Nil(t, mgr.Baseline(), "baseline should be nil before first Load/Commit")

	outcomes := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: synctypes.ItemTypeFile,
		LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
		LocalMtime: 1, RemoteMtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	require.NotNil(t, mgr.Baseline(), "baseline should be populated after Commit")
	assert.Equal(t, 1, mgr.Baseline().Len())
}

// Validates: R-2.2
func TestGetDeltaToken_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	token, err := mgr.GetDeltaToken(ctx, "nonexistent-drive", "")
	require.NoError(t, err)
	assert.Empty(t, token)
}

// Validates: R-2.2
func TestLoad_NullableFields(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert a row with NULL parent_id, hashes, size, mtimes, etag directly.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
		 VALUES (?, ?, ?, NULL, ?, NULL, NULL, NULL, NULL, NULL, NULL, ?, NULL)`,
		"root", "d", "root-id", "root", time.Now().UnixNano(),
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

// seedConflict inserts a conflict via CommitOutcome and returns its UUID.
func seedConflict(t *testing.T, mgr *SyncStore, path, conflictType string) string {
	t.Helper()

	ctx := t.Context()

	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         path,
		DriveID:      driveid.New("d"),
		ItemID:       "item-" + path,
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: conflictType,
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Retrieve the UUID.
	var id string

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT id FROM conflicts WHERE path = ? ORDER BY detected_at DESC LIMIT 1", path,
	).Scan(&id)
	require.NoError(t, err, "retrieving conflict ID for %s", path)

	return id
}

// Validates: R-2.3.2
func TestListConflicts_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

// Validates: R-2.3.2
func TestListConflicts_WithConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	seedConflict(t, mgr, "a.txt", "edit_edit")
	seedConflict(t, mgr, "b.txt", "edit_delete")

	ctx := t.Context()

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 2)
	assert.Equal(t, "a.txt", conflicts[0].Path)
	assert.Equal(t, "b.txt", conflicts[1].Path)
}

// Validates: R-2.3.2
func TestListConflicts_OnlyUnresolved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	id := seedConflict(t, mgr, "resolved.txt", "edit_edit")
	seedConflict(t, mgr, "pending.txt", "edit_edit")

	ctx := t.Context()

	// Resolve the first conflict.
	require.NoError(t, mgr.ResolveConflict(ctx, id, synctypes.ResolutionKeepBoth))

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "pending.txt", conflicts[0].Path)
}

// Validates: R-2.3.2
func TestGetConflict_ByID(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	id := seedConflict(t, mgr, "lookup.txt", "create_create")
	ctx := t.Context()

	c, err := mgr.GetConflict(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, id, c.ID)
	assert.Equal(t, "lookup.txt", c.Path)
	assert.Equal(t, "create_create", c.ConflictType)
}

// Validates: R-2.3.2
func TestGetConflict_ByPath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	seedConflict(t, mgr, "bypath.txt", "edit_edit")
	ctx := t.Context()

	c, err := mgr.GetConflict(ctx, "bypath.txt")
	require.NoError(t, err)
	assert.Equal(t, "bypath.txt", c.Path)
}

func TestGetConflict_NotFound(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	_, err := mgr.GetConflict(ctx, "nonexistent")
	require.Error(t, err)
}

// Validates: R-2.3.2
func TestResolveConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	id := seedConflict(t, mgr, "resolve-me.txt", "edit_edit")
	ctx := t.Context()

	require.NoError(t, mgr.ResolveConflict(ctx, id, synctypes.ResolutionKeepLocal))

	// Verify resolution was recorded.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE id = ?", id,
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ResolutionKeepLocal, resolution)
	assert.Equal(t, "user", resolvedBy)
	assert.NotZero(t, resolvedAt)
}

func TestResolveConflict_AlreadyResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	id := seedConflict(t, mgr, "double-resolve.txt", "edit_edit")
	ctx := t.Context()

	// First resolve succeeds.
	require.NoError(t, mgr.ResolveConflict(ctx, id, synctypes.ResolutionKeepBoth))

	// Second resolve fails (already resolved).
	err := mgr.ResolveConflict(ctx, id, synctypes.ResolutionKeepLocal)
	require.Error(t, err)
}

// Validates: R-2.2
func TestLoad_ReturnsCachedBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed a baseline entry.
	outcomes := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: synctypes.ItemTypeFile,
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
	outcomes := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "first.txt", DriveID: driveid.New("d"), ItemID: "i1", ItemType: synctypes.ItemTypeFile,
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
	outcomes2 := []synctypes.Outcome{{
		Action: synctypes.ActionDownload, Success: true,
		Path: "second.txt", DriveID: driveid.New("d"), ItemID: "i2", ItemType: synctypes.ItemTypeFile,
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

// Validates: R-2.3.2
func TestCommitConflict_AutoResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionConflict,
		Success:         true,
		Path:            "auto-resolved.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "new-item",
		ParentID:        "root",
		ItemType:        synctypes.ItemTypeFile,
		LocalHash:       "local-h",
		RemoteHash:      "remote-h",
		LocalSize:       512,
		LocalSizeKnown:  true,
		RemoteSize:      512,
		RemoteSizeKnown: true,
		LocalMtime:      fixedTime.UnixNano(),
		RemoteMtime:     fixedTime.UnixNano(),
		ConflictType:    "edit_delete",
		ResolvedBy:      "auto",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify conflict row was inserted as already resolved.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE path = ?",
		"auto-resolved.txt",
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ResolutionKeepLocal, resolution)
	assert.Equal(t, "auto", resolvedBy)
	assert.NotZero(t, resolvedAt)

	// Verify baseline was also updated (auto-resolve upserts baseline).
	entry, ok := mgr.Baseline().GetByPath("auto-resolved.txt")
	require.True(t, ok, "baseline entry not found for auto-resolved conflict")
	assert.Equal(t, "new-item", entry.ItemID)
	assert.Equal(t, "local-h", entry.LocalHash)
}

// Validates: R-2.3.2
func TestListAllConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })

	// Seed an unresolved conflict.
	seedConflict(t, mgr, "unresolved.txt", "edit_edit")

	// Seed and resolve a conflict.
	resolvedID := seedConflict(t, mgr, "resolved-file.txt", "edit_delete")
	ctx := t.Context()

	require.NoError(t, mgr.ResolveConflict(ctx, resolvedID, synctypes.ResolutionKeepLocal))

	// ListConflicts should return only unresolved.
	unresolved, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, unresolved, 1)
	assert.Equal(t, "unresolved.txt", unresolved[0].Path)

	// ListAllConflicts should return both.
	all, err := mgr.ListAllConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	// Verify resolution fields are populated.
	var found bool

	for i := range all {
		if all[i].Path == "resolved-file.txt" {
			found = true
			assert.Equal(t, synctypes.ResolutionKeepLocal, all[i].Resolution)
			assert.Equal(t, "user", all[i].ResolvedBy)
			assert.NotZero(t, all[i].ResolvedAt)
		}
	}

	assert.True(t, found, "resolved-file.txt not found in ListAllConflicts results")
}

// ---------------------------------------------------------------------------
// CommitOutcome tests
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestCommitOutcome_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return fixedTime })

	outcome := synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "co-download.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i1",
		ParentID:        "p1",
		ItemType:        synctypes.ItemTypeFile,
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

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.Baseline().GetByPath("co-download.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i1", entry.ItemID)
	assert.Equal(t, "lh", entry.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
}

// Validates: R-2.2
func TestCommitOutcome_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := synctypes.Outcome{
		Action:          synctypes.ActionUpload,
		Success:         true,
		Path:            "co-upload.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i2",
		ItemType:        synctypes.ItemTypeFile,
		LocalHash:       "h",
		RemoteHash:      "h",
		LocalSize:       256,
		LocalSizeKnown:  true,
		RemoteSize:      256,
		RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.Baseline().GetByPath("co-upload.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i2", entry.ItemID)
}

// Validates: R-6.7.17
func TestCommitOutcome_PersistsSideAwareFileMetadata(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC) })

	outcome := synctypes.Outcome{
		Action:          synctypes.ActionUpload,
		Success:         true,
		Path:            "hashless.docx",
		DriveID:         driveid.New("d"),
		ItemID:          "i-side-aware",
		ItemType:        synctypes.ItemTypeFile,
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

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.Baseline().GetByPath("hashless.docx")
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
func TestCommitOutcome_PersistsZeroByteSizeAsKnownZero(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	outcome := synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "zero.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "i-zero",
		ItemType:        synctypes.ItemTypeFile,
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

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	var (
		localSize       sql.NullInt64
		remoteSize      sql.NullInt64
		storedLocalETag sql.NullString
	)

	err := mgr.DB().QueryRowContext(ctx,
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
	assert.Contains(t, err.Error(), "rebuild or migrate the drive state DB")
}

// Validates: R-2.2
func TestCommitOutcome_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed an entry first.
	seed := synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true,
		Path: "co-delete.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &seed))

	del := synctypes.Outcome{Action: synctypes.ActionLocalDelete, Success: true, Path: "co-delete.txt"}
	require.NoError(t, mgr.CommitOutcome(ctx, &del))

	_, ok := mgr.Baseline().GetByPath("co-delete.txt")
	assert.False(t, ok, "entry still exists after delete")
}

// Validates: R-2.2
func TestCommitOutcome_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// Seed original entry.
	seed := synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true,
		Path: "old/move.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p1",
		ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &seed))

	move := synctypes.Outcome{
		Action: synctypes.ActionLocalMove, Success: true,
		Path: "new/move.txt", OldPath: "old/move.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: synctypes.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 10, LocalSizeKnown: true,
		RemoteSize: 10, RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &move))

	_, ok := mgr.Baseline().GetByPath("old/move.txt")
	assert.False(t, ok, "old path still exists after move")

	entry, ok := mgr.Baseline().GetByPath("new/move.txt")
	require.True(t, ok, "new path not found after move")
	assert.Equal(t, "p2", entry.ParentID)
}

// Validates: R-2.3.2
func TestCommitOutcome_Conflict_AutoResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := synctypes.Outcome{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "co-conflict.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "new-item",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "lh",
		RemoteHash:   "rh",
		ConflictType: synctypes.ConflictEditEdit,
		ResolvedBy:   synctypes.ResolvedByAuto,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	// Auto-resolved conflict should update baseline.
	entry, ok := mgr.Baseline().GetByPath("co-conflict.txt")
	require.True(t, ok, "baseline entry not found for auto-resolved conflict")
	assert.Equal(t, "new-item", entry.ItemID)

	// Conflict row should exist.
	var resolution string

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT resolution FROM conflicts WHERE path = ?", "co-conflict.txt",
	).Scan(&resolution)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ResolutionKeepLocal, resolution)
}

// Validates: R-2.3.2
func TestCommitOutcome_Conflict_Unresolved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := synctypes.Outcome{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "co-unresolved.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     synctypes.ItemTypeFile,
		ConflictType: synctypes.ConflictEditEdit,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	// Unresolved conflict should NOT update baseline.
	_, ok := mgr.Baseline().GetByPath("co-unresolved.txt")
	assert.False(t, ok, "baseline entry should not exist for unresolved conflict")
}

// Validates: R-2.3.2
func TestCommitOutcome_EditDeleteConflict_DeletesBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	// First, create a baseline entry for the file.
	setupOutcome := synctypes.Outcome{
		Action:   synctypes.ActionDownload,
		Success:  true,
		Path:     "edit-delete-target.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "i1",
		ItemType: synctypes.ItemTypeFile,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &setupOutcome))

	// Verify baseline entry exists.
	_, ok := mgr.Baseline().GetByPath("edit-delete-target.txt")
	require.True(t, ok, "baseline entry should exist after download")

	// Now commit an unresolved edit-delete conflict (B-133).
	conflictOutcome := synctypes.Outcome{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "edit-delete-target.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i1",
		ItemType:     synctypes.ItemTypeFile,
		ConflictType: synctypes.ConflictEditDelete,
		LocalHash:    "modified-hash",
		RemoteHash:   "baseline-remote-hash",
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &conflictOutcome))

	// Baseline entry should be deleted — the original file was renamed to conflict copy.
	_, ok = mgr.Baseline().GetByPath("edit-delete-target.txt")
	assert.False(t, ok, "baseline entry should be deleted for unresolved edit-delete conflict")

	// Conflict record should exist.
	var conflictType, resolution string

	err := mgr.DB().QueryRowContext(ctx,
		"SELECT conflict_type, resolution FROM conflicts WHERE path = ?", "edit-delete-target.txt",
	).Scan(&conflictType, &resolution)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictEditDelete, conflictType)
	assert.Equal(t, synctypes.ResolutionUnresolved, resolution)
}

// Validates: R-2.2
func TestCommitOutcome_SkipsFailedOutcome(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	outcome := synctypes.Outcome{
		Action:  synctypes.ActionDownload,
		Success: false,
		Path:    "should-not-exist.txt",
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	if mgr.Baseline() != nil {
		_, ok := mgr.Baseline().GetByPath("should-not-exist.txt")
		assert.False(t, ok, "failed outcome should not create baseline entry")
	}
}

// Validates: R-2.2
func TestCommitOutcome_UnknownAction_ReturnsErrorAndSkipsDBWrite(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	err := mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action:   synctypes.ActionType(999),
		Success:  true,
		Path:     "unknown.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "unknown-item",
		ItemType: synctypes.ItemTypeFile,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown action type")

	var count int
	queryErr := mgr.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline WHERE path = ?`, "unknown.txt").Scan(&count)
	require.NoError(t, queryErr)
	assert.Zero(t, count, "unknown actions must not write baseline rows")

	_, ok := mgr.Baseline().GetByPath("unknown.txt")
	assert.False(t, ok, "unknown actions must not mutate the in-memory cache")
}

// Validates: R-2.2
func TestUpdateBaselineCache_UnknownActionReloadsFromDB(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	seed := synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "cached.txt",
		DriveID:         driveid.New("d"),
		ItemID:          "item-cached",
		ItemType:        synctypes.ItemTypeFile,
		LocalHash:       "hash1",
		RemoteHash:      "hash1",
		LocalSize:       42,
		LocalSizeKnown:  true,
		RemoteSize:      42,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &seed))

	mgr.Baseline().Delete("cached.txt")
	_, ok := mgr.Baseline().GetByPath("cached.txt")
	require.False(t, ok, "test setup should corrupt the cache before reload")

	err := mgr.updateBaselineCache(ctx, &synctypes.Outcome{
		Action: synctypes.ActionType(999),
		Path:   "ignored.txt",
	}, time.Now().UnixNano())
	require.NoError(t, err)

	entry, ok := mgr.Baseline().GetByPath("cached.txt")
	require.True(t, ok, "unknown cache mutations should trigger a DB reload")
	assert.Equal(t, "item-cached", entry.ItemID)
}

// Validates: R-2.2
func TestCommitOutcome_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	outcome := synctypes.Outcome{
		Action:   synctypes.ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("d"),
		ItemID:   "folder-id",
		ParentID: "root",
		ItemType: synctypes.ItemTypeFolder,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.Baseline().GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, synctypes.ItemTypeFolder, entry.ItemType)
	assert.Equal(t, "folder-id", entry.ItemID)
}

// Validates: R-2.2
// TestCommitOutcome_Upload_NewItemID_SamePath verifies that when an upload
// outcome has a different item_id than the existing baseline entry at the same
// path (e.g., server-side delete+recreate assigns new ID), the stale row is
// replaced and no UNIQUE constraint violation occurs.
func TestCommitOutcome_Upload_NewItemID_SamePath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	driveID := driveid.New("d1")

	// Seed baseline with item_id "old-id" at path "file.txt".
	seedOutcome := synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "old-id",
		ItemType:        synctypes.ItemTypeFile,
		RemoteHash:      "hash1",
		LocalHash:       "hash1",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &seedOutcome))

	// Upload outcome with different item_id for the same path.
	uploadOutcome := synctypes.Outcome{
		Action:          synctypes.ActionUpload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveID,
		ItemID:          "new-id",
		ItemType:        synctypes.ItemTypeFile,
		RemoteHash:      "hash2",
		LocalHash:       "hash2",
		LocalSize:       200,
		LocalSizeKnown:  true,
		RemoteSize:      200,
		RemoteSizeKnown: true,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &uploadOutcome))

	// Verify the entry now has the new item_id.
	entry, ok := mgr.Baseline().GetByPath("file.txt")
	require.True(t, ok, "entry should exist")
	assert.Equal(t, "new-id", entry.ItemID)
	assert.Equal(t, "hash2", entry.RemoteHash)

	// Old ID should no longer exist in ByID.
	_, ok = mgr.Baseline().GetByID(driveid.NewItemKey(driveID, "old-id"))
	assert.False(t, ok, "old ID should be removed from ByID")

	// New ID should exist in ByID.
	_, ok = mgr.Baseline().GetByID(driveid.NewItemKey(driveID, "new-id"))
	assert.True(t, ok, "new ID should exist in ByID")
}

// ---------------------------------------------------------------------------
// CommitDeltaToken tests
// ---------------------------------------------------------------------------

// Validates: R-2.15.1
func TestCommitDeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-abc", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-abc", token)
}

// Validates: R-2.15.1
func TestCommitDeltaToken_Update(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	mgr.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })

	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-1", "d", "", "d"))
	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-2", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
}

// Validates: R-2.15.1
func TestCommitDeltaToken_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Empty token should be a no-op.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Empty(t, token)
}

// Validates: R-2.15.1
func TestCommitDeltaToken_CompositeKey_DifferentScopes(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Primary delta (scope_id="").
	require.NoError(t, mgr.CommitDeltaToken(ctx, "primary-token", "drv1", "", "drv1"))

	// Shortcut-scoped delta (scope_id=remoteItemID, scope_drive=remoteDriveID).
	require.NoError(t, mgr.CommitDeltaToken(ctx, "shortcut-token", "drv1", "item123", "drv2"))

	// Both should be independently retrievable.
	primary, err := mgr.GetDeltaToken(ctx, "drv1", "")
	require.NoError(t, err)
	assert.Equal(t, "primary-token", primary)

	shortcut, err := mgr.GetDeltaToken(ctx, "drv1", "item123")
	require.NoError(t, err)
	assert.Equal(t, "shortcut-token", shortcut)
}

// Validates: R-2.15.1
func TestCommitDeltaToken_CompositeKey_UpdatePreservesOtherScopes(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Save two scoped tokens.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "tok-a", "drv1", "", "drv1"))
	require.NoError(t, mgr.CommitDeltaToken(ctx, "tok-b", "drv1", "scope1", "drv2"))

	// Update only the primary.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "tok-a-updated", "drv1", "", "drv1"))

	// Primary should be updated.
	primary, err := mgr.GetDeltaToken(ctx, "drv1", "")
	require.NoError(t, err)
	assert.Equal(t, "tok-a-updated", primary)

	// Other scope should be unchanged.
	scoped, err := mgr.GetDeltaToken(ctx, "drv1", "scope1")
	require.NoError(t, err)
	assert.Equal(t, "tok-b", scoped)
}

// ---------------------------------------------------------------------------
// Locked accessor tests (Baseline.GetByPath, GetByID, Put, Delete, Len, ForEachPath)
// ---------------------------------------------------------------------------

// Validates: R-2.2
func TestBaseline_GetByPath(t *testing.T) {
	t.Parallel()

	b := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"docs/readme.md": {Path: "docs/readme.md", ItemID: "item1", DriveID: driveid.New("d1")},
		},
		ByID:       make(map[driveid.ItemKey]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
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
	key := driveid.NewItemKey(driveID, "item1")
	entry := &synctypes.BaselineEntry{Path: "test.txt", ItemID: "item1", DriveID: driveID}

	b := &synctypes.Baseline{
		ByPath:     make(map[string]*synctypes.BaselineEntry),
		ByID:       map[driveid.ItemKey]*synctypes.BaselineEntry{key: entry},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	got, ok := b.GetByID(key)
	require.True(t, ok)
	assert.Equal(t, "test.txt", got.Path)

	missingKey := driveid.NewItemKey(driveID, "nonexistent")
	_, ok = b.GetByID(missingKey)
	assert.False(t, ok)
}

// Validates: R-2.2
func TestBaseline_Put(t *testing.T) {
	t.Parallel()

	b := &synctypes.Baseline{
		ByPath:     make(map[string]*synctypes.BaselineEntry),
		ByID:       make(map[driveid.ItemKey]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	entry := &synctypes.BaselineEntry{
		Path:    "new/file.txt",
		DriveID: driveid.New("d1"),
		ItemID:  "item-new",
	}

	b.Put(entry)

	got, ok := b.GetByPath("new/file.txt")
	require.True(t, ok, "entry not found after Put")
	assert.Equal(t, "item-new", got.ItemID)

	key := driveid.NewItemKey(driveid.New("d1"), "item-new")
	gotByID, ok := b.GetByID(key)
	require.True(t, ok, "entry not found by ID after Put")
	assert.Equal(t, "new/file.txt", gotByID.Path)
}

// TestBaseline_Put_ReplacesStaleID verifies that Put at an existing path with
// a different (driveID, itemID) removes the stale ByID entry.
// Validates: R-2.2
func TestBaseline_Put_ReplacesStaleID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	oldEntry := &synctypes.BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "old-id"}
	oldKey := driveid.NewItemKey(driveID, "old-id")

	b := &synctypes.Baseline{
		ByPath:     map[string]*synctypes.BaselineEntry{"file.txt": oldEntry},
		ByID:       map[driveid.ItemKey]*synctypes.BaselineEntry{oldKey: oldEntry},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	// Put a new entry at the same path but different item_id.
	newEntry := &synctypes.BaselineEntry{Path: "file.txt", DriveID: driveID, ItemID: "new-id"}
	b.Put(newEntry)

	// New entry should be accessible by path and new ID.
	got, ok := b.GetByPath("file.txt")
	require.True(t, ok, "entry not found by path")
	assert.Equal(t, "new-id", got.ItemID)

	newKey := driveid.NewItemKey(driveID, "new-id")
	_, ok = b.GetByID(newKey)
	assert.True(t, ok, "new ID not found in ByID")

	// Old ID should be gone.
	_, ok = b.GetByID(oldKey)
	assert.False(t, ok, "stale old ID should be removed from ByID")

	// Only 1 entry total.
	assert.Equal(t, 1, b.Len())
}

// Validates: R-2.2
func TestBaseline_Delete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	entry := &synctypes.BaselineEntry{Path: "delete-me.txt", DriveID: driveID, ItemID: "item-del"}
	key := driveid.NewItemKey(driveID, "item-del")

	b := &synctypes.Baseline{
		ByPath:     map[string]*synctypes.BaselineEntry{"delete-me.txt": entry},
		ByID:       map[driveid.ItemKey]*synctypes.BaselineEntry{key: entry},
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	b.Delete("delete-me.txt")

	_, ok := b.GetByPath("delete-me.txt")
	assert.False(t, ok, "entry still exists after Delete")

	_, ok = b.GetByID(key)
	assert.False(t, ok, "entry still exists in ByID after Delete")

	// Deleting nonexistent path should not panic.
	b.Delete("nonexistent")
}

// Validates: R-2.2
func TestBaseline_Len(t *testing.T) {
	t.Parallel()

	b := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"a.txt": {Path: "a.txt"},
			"b.txt": {Path: "b.txt"},
		},
		ByID:       make(map[driveid.ItemKey]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	assert.Equal(t, 2, b.Len())
}

// Validates: R-2.2
func TestBaseline_ForEachPath(t *testing.T) {
	t.Parallel()

	b := &synctypes.Baseline{
		ByPath: map[string]*synctypes.BaselineEntry{
			"a.txt": {Path: "a.txt", ItemID: "i1"},
			"b.txt": {Path: "b.txt", ItemID: "i2"},
		},
		ByID:       make(map[driveid.ItemKey]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	paths := make(map[string]bool)
	b.ForEachPath(func(path string, entry *synctypes.BaselineEntry) {
		paths[path] = true
	})

	assert.Len(t, paths, 2)
	assert.True(t, paths["a.txt"], "ForEachPath did not visit a.txt")
	assert.True(t, paths["b.txt"], "ForEachPath did not visit b.txt")
}

// Validates: R-2.2, R-6.4
func TestBaseline_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := &synctypes.Baseline{
		ByPath:     make(map[string]*synctypes.BaselineEntry),
		ByID:       make(map[driveid.ItemKey]*synctypes.BaselineEntry),
		ByDirLower: make(map[synctypes.DirLowerKey][]*synctypes.BaselineEntry),
	}

	// Seed some entries.
	for i := range 100 {
		entry := &synctypes.BaselineEntry{
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
			entry := &synctypes.BaselineEntry{
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
			b.ForEachPath(func(_ string, _ *synctypes.BaselineEntry) {})
		}()
	}

	wg.Wait()

	// Baseline should have at least original + concurrent entries.
	assert.GreaterOrEqual(t, b.Len(), 100)
}

// TestConflictRecord_NameField verifies that ConflictRecord.Name is populated
// as path.Base(Path) by the shared scanConflict function (B-071).
// Validates: R-2.3.2
func TestConflictRecord_NameField(t *testing.T) {
	mgr := newTestStore(t)
	ctx := t.Context()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a conflict with a nested path.
	outcome := &synctypes.Outcome{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "docs/notes/readme.md",
		DriveID:      driveid.New(testDriveID),
		ItemID:       "item-name-test",
		ConflictType: synctypes.ConflictEditEdit,
		LocalHash:    "localH",
		RemoteHash:   "remoteH",
		LocalMtime:   100,
		RemoteMtime:  200,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, outcome))

	// Verify via ListConflicts.
	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "readme.md", conflicts[0].Name)

	// Verify via GetConflict.
	c, err := mgr.GetConflict(ctx, conflicts[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "readme.md", c.Name)
}

// TestPruneResolvedConflicts verifies that PruneResolvedConflicts deletes
// resolved conflicts older than the retention period while preserving
// newer resolved and all unresolved conflicts (B-087).
// setupPruneTestConflicts populates a test manager with three conflicts:
// - An "old" resolved conflict (detected 120 days ago)
// - A "new" resolved conflict (detected 10 days ago)
// - An unresolved conflict (detected 120 days ago)
// Returns the IDs of the old and new resolved conflicts.
func setupPruneTestConflicts(t *testing.T, mgr *SyncStore, ctx context.Context, now time.Time) (oldID, newID string) {
	t.Helper()

	mgr.SetNowFunc(func() time.Time { return now.AddDate(0, 0, -120) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionConflict, Success: true,
		Path: "old-resolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-old", ConflictType: synctypes.ConflictEditEdit,
		LocalHash: "lh1", RemoteHash: "rh1", LocalMtime: 100, RemoteMtime: 200,
	}))

	mgr.SetNowFunc(func() time.Time { return now.AddDate(0, 0, -100) })

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)

	oldID = conflicts[0].ID
	require.NoError(t, mgr.ResolveConflict(ctx, oldID, "keep_local"))

	mgr.SetNowFunc(func() time.Time { return now.AddDate(0, 0, -10) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionConflict, Success: true,
		Path: "new-resolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-new", ConflictType: synctypes.ConflictEditEdit,
		LocalHash: "lh2", RemoteHash: "rh2", LocalMtime: 300, RemoteMtime: 400,
	}))

	mgr.SetNowFunc(func() time.Time { return now.AddDate(0, 0, -5) })

	conflicts, err = mgr.ListConflicts(ctx)
	require.NoError(t, err)

	for i := range conflicts {
		if conflicts[i].Path == "new-resolved.txt" {
			newID = conflicts[i].ID
		}
	}

	require.NoError(t, mgr.ResolveConflict(ctx, newID, "keep_remote"))

	mgr.SetNowFunc(func() time.Time { return now.AddDate(0, 0, -120) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionConflict, Success: true,
		Path: "unresolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-unresolved", ConflictType: synctypes.ConflictEditEdit,
		LocalHash: "lh3", RemoteHash: "rh3", LocalMtime: 500, RemoteMtime: 600,
	}))

	return oldID, newID
}

// TestPruneResolvedConflicts verifies that PruneResolvedConflicts deletes
// resolved conflicts older than the retention period while preserving
// newer resolved and all unresolved conflicts (B-087).
// Validates: R-2.3.2
func TestPruneResolvedConflicts(t *testing.T) {
	mgr := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	mgr.SetNowFunc(func() time.Time { return now })

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	oldID, newID := setupPruneTestConflicts(t, mgr, ctx, now)

	mgr.SetNowFunc(func() time.Time { return now })

	pruned, err := mgr.PruneResolvedConflicts(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned, "only old resolved should be pruned")

	// Old resolved should be gone.
	c, err := mgr.GetConflict(ctx, oldID)
	assert.True(t, err != nil || c == nil, "old resolved conflict should have been pruned")

	// New resolved should still exist.
	c, err = mgr.GetConflict(ctx, newID)
	require.NoError(t, err)
	assert.Equal(t, "keep_remote", c.Resolution)

	// Unresolved conflict should still exist.
	unresolved, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, unresolved, 1)
	assert.Equal(t, "unresolved.txt", unresolved[0].Path)
}

// Validates: R-2.2
// TestCheckCacheConsistency verifies that CheckCacheConsistency detects
// mismatches between the in-memory baseline cache and the database (B-198).
func TestCheckCacheConsistency(t *testing.T) {
	mgr := newTestStore(t)
	ctx := t.Context()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a baseline entry via CommitOutcome.
	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true,
		Path: "consistency-check.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-cc", ParentID: "root", ItemType: synctypes.ItemTypeFile,
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
	_, err = mgr.DB().ExecContext(ctx,
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
		"baseline", "delta_tokens", "conflicts", "sync_metadata",
		"remote_state", "sync_failures",
	}

	for _, table := range expectedTables {
		var name string
		err := mgr.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		require.NoError(t, err, "table %q should exist", table)
		assert.Equal(t, table, name)
	}

	// Verify delta_tokens composite key: two tokens for same drive_id,
	// different scope_id should coexist.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, cursor, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"d!abc123", "", "d!abc123", "primary-token", 1700000000)
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, cursor, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"d!abc123", "shared-folder-id", "d!other456", "scoped-token", 1700000001)
	require.NoError(t, err)

	var count int
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM delta_tokens WHERE drive_id = ?`, "d!abc123",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Verify remote_state table structure: insert + query.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item1", "/test.txt", "file", synctypes.SyncStatusPendingDownload, 1700000000)
	require.NoError(t, err)

	// Verify sync_failures table structure: insert + query.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, issue_type, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"/bad-file.txt", "d!abc123", "upload", "upload", "item", "transient", "invalid_filename", 1700000000, 1700000000)
	require.NoError(t, err)

	// Verify remote_state CHECK constraint rejects invalid status.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item2", "/bad.txt", "file", "invalid_status", 1700000000)
	require.Error(t, err, "invalid sync_status should be rejected by CHECK constraint")
}

// Validates: R-2.2
func TestConsolidatedSchema_RemoteStateActivePathUnique(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert an active item at a path.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item1", "/test.txt", "file", synctypes.SyncStatusSynced, 1700000000)
	require.NoError(t, err)

	// Another active item at the same path should be rejected by the partial unique index.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item2", "/test.txt", "file", synctypes.SyncStatusPendingDownload, 1700000000)
	require.Error(t, err, "duplicate active path should be rejected")

	// A deleted item at the same path should be allowed.
	_, err = mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item3", "/test.txt", "file", synctypes.SyncStatusDeleted, 1700000000)
	require.NoError(t, err, "deleted item at same path should be allowed")
}

// --- Sync metadata tests (6.2b) ---

// Validates: R-2.2
func TestWriteSyncMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	report := &synctypes.SyncReport{
		Duration:  1500 * time.Millisecond,
		Succeeded: 42,
		Failed:    3,
		Errors:    []error{fmt.Errorf("some sync error")},
	}

	require.NoError(t, mgr.WriteSyncMetadata(ctx, report))

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, "1500", meta["last_sync_duration_ms"])
	assert.Equal(t, "42", meta["last_sync_succeeded"])
	assert.Equal(t, "3", meta["last_sync_failed"])
	assert.Equal(t, "some sync error", meta["last_sync_error"])
	assert.NotEmpty(t, meta["last_sync_time"])
}

// Validates: R-2.2
func TestWriteSyncMetadata_Upsert(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	report1 := &synctypes.SyncReport{Duration: 1 * time.Second, Succeeded: 10}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report1))

	report2 := &synctypes.SyncReport{Duration: 2 * time.Second, Succeeded: 20}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report2))

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, "20", meta["last_sync_succeeded"], "should be from second write")
	assert.Equal(t, "2000", meta["last_sync_duration_ms"])
}

// Validates: R-2.2
func TestWriteSyncMetadata_NoErrors(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	report := &synctypes.SyncReport{Duration: 500 * time.Millisecond, Succeeded: 5}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report))

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Empty(t, meta["last_sync_error"])
}

// Validates: R-2.2
func TestReadSyncMetadata_EmptyDB(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Empty(t, meta)
}

// Validates: R-2.2
func TestReadSyncMetadata_MissingTableReturnsEmpty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	_, err := mgr.db.ExecContext(ctx, `DROP TABLE sync_metadata`)
	require.NoError(t, err)

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Empty(t, meta)
}

// Validates: R-2.2
func TestBaselineEntryCount_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	count, err := mgr.BaselineEntryCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// Validates: R-2.2
func TestBaselineEntryCount_WithEntries(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Commit a download outcome to add a baseline entry.
	outcome := synctypes.Outcome{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "/test/file.txt",
		DriveID:         driveid.New("d!123"),
		ItemID:          "item-1",
		ParentID:        "parent-1",
		ItemType:        synctypes.ItemTypeFile,
		RemoteHash:      "hash123",
		LocalSize:       1024,
		LocalSizeKnown:  true,
		RemoteSize:      1024,
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	count, err := mgr.BaselineEntryCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// Validates: R-2.3.2
func TestUnresolvedConflictCount_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	count, err := mgr.UnresolvedConflictCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// --- SetDispatchStatus tests (5.7.2) ---

// Validates: R-2.5
func TestSetDispatchStatus_Transitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		initialStatus synctypes.SyncStatus
		action        synctypes.ActionType
		wantStatus    synctypes.SyncStatus
	}{
		{
			name:          "Download",
			initialStatus: synctypes.SyncStatusPendingDownload,
			action:        synctypes.ActionDownload,
			wantStatus:    synctypes.SyncStatusDownloading,
		},
		{
			name:          "DownloadFromFailed",
			initialStatus: synctypes.SyncStatusDownloadFailed,
			action:        synctypes.ActionDownload,
			wantStatus:    synctypes.SyncStatusDownloading,
		},
		{
			name:          "LocalDelete",
			initialStatus: synctypes.SyncStatusPendingDelete,
			action:        synctypes.ActionLocalDelete,
			wantStatus:    synctypes.SyncStatusDeleting,
		},
		{
			name:          "DeleteFromFailed",
			initialStatus: synctypes.SyncStatusDeleteFailed,
			action:        synctypes.ActionLocalDelete,
			wantStatus:    synctypes.SyncStatusDeleting,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()

			_, err := mgr.DB().ExecContext(ctx,
				`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				"d!abc", "item1", "/test.txt", "file", tc.initialStatus, 1700000000)
			require.NoError(t, err)

			require.NoError(t, mgr.SetDispatchStatus(ctx, "d!abc", "item1", tc.action))

			var status synctypes.SyncStatus
			err = mgr.DB().QueryRowContext(ctx,
				`SELECT sync_status FROM remote_state WHERE drive_id = ? AND item_id = ?`,
				"d!abc", "item1").Scan(&status)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, status)
		})
	}
}

// Validates: R-2.5
func TestSetDispatchStatus_NoMatchingRow(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Insert a synced row (wrong status for dispatch).
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "/test.txt", "file", synctypes.SyncStatusSynced, 1700000000)
	require.NoError(t, err)

	// Should be a no-op (no error, no change).
	require.NoError(t, mgr.SetDispatchStatus(ctx, "d!abc", "item1", synctypes.ActionDownload))

	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		"d!abc", "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusSynced, status, "synced row should not be affected")
}

// Validates: R-2.5
func TestSetDispatchStatus_UnsupportedAction(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Unsupported action type — should be a no-op.
	require.NoError(t, mgr.SetDispatchStatus(ctx, "d!abc", "item1", synctypes.ActionUpload))
}

// Validates: R-2.5
func TestSetDispatchStatus_NonExistentRow(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// No rows exist — should be a no-op.
	require.NoError(t, mgr.SetDispatchStatus(ctx, "d!nonexistent", "noitem", synctypes.ActionDownload))
}

// --- Enhanced crash recovery tests (5.7.2) ---

// Validates: R-2.5.1, R-6.5.2
func TestResetInProgressStates_DeleteFileAbsent(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	syncRoot := t.TempDir()

	// Insert a deleting row whose file does NOT exist on disk.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "gone.txt", "file", synctypes.SyncStatusDeleting, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		"d!abc", "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, status, "file absent → deleted")
}

// Validates: R-2.5.1, R-6.5.2
func TestResetInProgressStates_DeleteFileExists(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	syncRoot := t.TempDir()

	// Create the file on disk.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "exists.txt"), []byte("data"), 0o600))

	// Insert a deleting row whose file DOES exist.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "exists.txt", "file", synctypes.SyncStatusDeleting, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		"d!abc", "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, status, "file exists → pending_delete")
}

// Validates: R-2.5.1, R-6.5.2
func TestResetInProgressStates_DownloadStillResetsToPending(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	syncRoot := t.TempDir()

	// Insert a downloading row.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "dl.txt", "file", synctypes.SyncStatusDownloading, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE drive_id = ? AND item_id = ?`,
		"d!abc", "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, status)
}

// Validates: R-2.5.1, R-6.5.2
func TestResetInProgressStates_MixedStates(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	syncRoot := t.TempDir()

	// Create a file for one of the deleting items.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "has-file.txt"), []byte("data"), 0o600))

	// Insert: downloading, deleting (file absent), deleting (file exists), synced.
	for _, row := range []struct {
		id     string
		path   string
		status synctypes.SyncStatus
	}{
		{"i1", "downloading.txt", synctypes.SyncStatusDownloading},
		{"i2", "no-file.txt", synctypes.SyncStatusDeleting},
		{"i3", "has-file.txt", synctypes.SyncStatusDeleting},
		{"i4", "synced.txt", synctypes.SyncStatusSynced},
	} {
		_, err := mgr.DB().ExecContext(ctx,
			`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			"d!abc", row.id, row.path, "file", row.status, 1700000000)
		require.NoError(t, err)
	}

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	// Verify each row.
	expected := map[string]synctypes.SyncStatus{
		"i1": synctypes.SyncStatusPendingDownload, // downloading → pending_download
		"i2": synctypes.SyncStatusDeleted,         // deleting + file absent → deleted
		"i3": synctypes.SyncStatusPendingDelete,   // deleting + file exists → pending_delete
		"i4": synctypes.SyncStatusSynced,          // untouched
	}
	for id, want := range expected {
		var got synctypes.SyncStatus
		err := mgr.DB().QueryRowContext(ctx,
			`SELECT sync_status FROM remote_state WHERE item_id = ?`, id).Scan(&got)
		require.NoError(t, err)
		assert.Equal(t, want, got, "item %s", id)
	}
}

// --- Crash recovery → sync_failures bridge tests (R-2.5.4) ---

// Validates: R-2.5.4
func TestResetInProgressStates_CreatesSyncFailures_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	syncRoot := t.TempDir()

	// Insert a downloading row.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "dl.txt", "file", synctypes.SyncStatusDownloading, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	// Verify remote_state was reset.
	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = ?`, "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, status)

	// Verify sync_failures entry was created.
	failures, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "dl.txt", failures[0].Path)
	assert.Equal(t, synctypes.DirectionDownload, failures[0].Direction)
	assert.Equal(t, synctypes.CategoryTransient, failures[0].Category)
	assert.Equal(t, 1, failures[0].FailureCount)
	assert.Contains(t, failures[0].LastError, "crash recovery")
}

// Validates: R-2.5.4
func TestResetInProgressStates_CreatesSyncFailures_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	syncRoot := t.TempDir()

	// Create the file so it transitions to pending_delete (not deleted).
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "del.txt"), []byte("data"), 0o600))

	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "del.txt", "file", synctypes.SyncStatusDeleting, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	// Verify sync_failures entry was created for delete direction.
	failures, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "del.txt", failures[0].Path)
	assert.Equal(t, synctypes.DirectionDelete, failures[0].Direction)
	assert.Equal(t, synctypes.ActionLocalDelete, failures[0].ActionType)
	assert.Equal(t, synctypes.CategoryTransient, failures[0].Category)
}

// Validates: R-2.5.4
func TestResetInProgressStates_NoSyncFailure_DeleteComplete(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	syncRoot := t.TempDir()

	// File absent → delete completed before crash → no sync_failures entry.
	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc", "item1", "gone.txt", "file", synctypes.SyncStatusDeleting, 1700000000)
	require.NoError(t, err)

	testDelay := func(_ int) time.Duration { return time.Second }
	resetInProgressStates(t, mgr, syncRoot, testDelay)

	// Remote_state should be "deleted".
	var status synctypes.SyncStatus
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = ?`, "item1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, status)

	// No sync_failures should exist.
	failures, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "completed delete should not create sync_failures")
}
