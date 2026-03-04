package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// testLogger returns a debug-level logger that writes to t.Log,
// so all activity appears in CI output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(newTestLogWriter(t), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// testLogWriter adapts testing.T to io.Writer for slog. Uses a done channel
// to silently discard writes after the test has finished, preventing the race
// between t.Log() and testing.T's internal cleanup (goroutines spawned during
// the test may still log after the test function returns).
type testLogWriter struct {
	t    *testing.T
	done chan struct{}
	once stdsync.Once
}

func newTestLogWriter(t *testing.T) *testLogWriter {
	t.Helper()

	w := &testLogWriter{t: t, done: make(chan struct{})}
	t.Cleanup(func() { w.once.Do(func() { close(w.done) }) })

	return w
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	select {
	case <-w.done:
		// Test finished — discard to avoid t.Log() race.
		return len(p), nil
	default:
	}

	w.t.Helper()
	w.t.Log(string(p))

	return len(p), nil
}

// commitAll is a test helper that commits outcomes one by one via CommitOutcome.
func commitAll(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []Outcome) {
	t.Helper()
	for i := range outcomes {
		require.NoError(t, mgr.CommitOutcome(ctx, &outcomes[i]), "CommitOutcome[%d]", i)
	}
}

// newTestManager creates a SyncStore backed by a temp directory,
// registering cleanup with t.Cleanup.
func newTestManager(t *testing.T) *SyncStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	mgr, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err, "NewSyncStore(%q)", dbPath)

	t.Cleanup(func() {
		assert.NoError(t, mgr.Close(), "Close()")
	})

	return mgr
}

func TestNewSyncStore_CreatesDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	mgr, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err)
	defer mgr.Close()

	// Verify DB file exists by opening a direct connection.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.PingContext(t.Context()))
}

func TestNewSyncStore_WALMode(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)

	var journalMode string

	ctx := t.Context()
	err := mgr.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "wal", journalMode)
}

func TestSyncStore_Close_CheckpointsWAL(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	mgr, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err)

	// Write some data to ensure WAL has content.
	ctx := t.Context()
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, size, mtime, synced_at, etag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"/test.txt", "drv1", "item1", "parent1", "file",
		"hash1", "hash1", 100, 1700000000, 1700000000, "etag1")
	require.NoError(t, err)

	// Close should checkpoint and remove the WAL file.
	require.NoError(t, mgr.Close())

	// After TRUNCATE checkpoint, the WAL file should be empty or absent.
	walPath := dbPath + "-wal"
	info, statErr := os.Stat(walPath)
	if statErr == nil {
		// WAL file exists but should be empty after TRUNCATE.
		assert.Zero(t, info.Size(), "WAL file should be empty after TRUNCATE checkpoint")
	}
	// If WAL file doesn't exist at all, that's also fine.
}

func TestNewSyncStore_RunsMigrations(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)

	// goose creates a goose_db_version table automatically.
	ctx := t.Context()

	var count int

	err := mgr.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0",
	).Scan(&count)
	require.NoError(t, err)
	assert.NotZero(t, count, "no migrations applied (goose_db_version has no entries)")
}

func TestLoad_EmptyBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	b, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, b.ByPath)
	assert.Empty(t, b.ByID)
}

func TestCommit_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action:     ActionDownload,
		Success:    true,
		Path:       "docs/readme.md",
		DriveID:    driveid.New("drive1"),
		ItemID:     "item1",
		ParentID:   "parent1",
		ItemType:   ItemTypeFile,
		LocalHash:  "abc123",
		RemoteHash: "abc123",
		Size:       1024,
		Mtime:      fixedTime.Add(-time.Hour).UnixNano(),
		ETag:       "etag1",
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry := mgr.baseline.ByPath["docs/readme.md"]
	require.NotNil(t, entry, "baseline entry not found for docs/readme.md")
	assert.True(t, entry.DriveID.Equal(driveid.New("drive1")), "DriveID mismatch")
	assert.Equal(t, "item1", entry.ItemID)
	assert.Equal(t, "abc123", entry.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
}

func TestCommit_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action:     ActionUpload,
		Success:    true,
		Path:       "photos/cat.jpg",
		DriveID:    driveid.New("drive2"),
		ItemID:     "item2",
		ParentID:   "parent2",
		ItemType:   ItemTypeFile,
		LocalHash:  "hash-local",
		RemoteHash: "hash-remote",
		Size:       2048,
		Mtime:      fixedTime.UnixNano(),
		ETag:       "etag2",
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry := mgr.baseline.ByPath["photos/cat.jpg"]
	require.NotNil(t, entry, "baseline entry not found")
	assert.Equal(t, "hash-local", entry.LocalHash)
	assert.Equal(t, "hash-remote", entry.RemoteHash)
}

func TestCommit_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("drive1"),
		ItemID:   "folder1",
		ParentID: "root",
		ItemType: ItemTypeFolder,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry := mgr.baseline.ByPath["Documents/Reports"]
	require.NotNil(t, entry, "folder entry not found")
	assert.Equal(t, ItemTypeFolder, entry.ItemType)
	// Folders have no hash or size.
	assert.Empty(t, entry.LocalHash)
	assert.Zero(t, entry.Size)
}

func TestCommit_UpdateSynced(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// First commit: create baseline entry.
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return t1 }

	outcomes := []Outcome{{
		Action:     ActionDownload,
		Success:    true,
		Path:       "file.txt",
		DriveID:    driveid.New("d"),
		ItemID:     "i",
		ItemType:   ItemTypeFile,
		LocalHash:  "h1",
		RemoteHash: "h1",
		Size:       100,
		Mtime:      t1.UnixNano(),
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Second commit: convergent edit updates synced_at.
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return t2 }

	outcomes[0].Action = ActionUpdateSynced
	outcomes[0].LocalHash = "h2"
	outcomes[0].RemoteHash = "h2"

	commitAll(t, mgr, ctx, outcomes)

	entry := mgr.baseline.ByPath["file.txt"]
	assert.Equal(t, t2.UnixNano(), entry.SyncedAt)
	assert.Equal(t, "h2", entry.LocalHash)
}

func TestCommit_LocalDelete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Create, then delete.
	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "delete-me.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 50, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	del := []Outcome{{
		Action: ActionLocalDelete, Success: true, Path: "delete-me.txt",
	}}

	commitAll(t, mgr, ctx, del)

	assert.Nil(t, mgr.baseline.ByPath["delete-me.txt"], "entry still exists after local delete")
}

func TestCommit_RemoteDelete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "remote-del.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 50, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	del := []Outcome{{
		Action: ActionRemoteDelete, Success: true, Path: "remote-del.txt",
	}}

	commitAll(t, mgr, ctx, del)

	assert.Nil(t, mgr.baseline.ByPath["remote-del.txt"], "entry still exists after remote delete")
}

func TestCommit_Cleanup(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "cleanup.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 50, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	cleanup := []Outcome{{
		Action: ActionCleanup, Success: true, Path: "cleanup.txt",
	}}

	commitAll(t, mgr, ctx, cleanup)

	assert.Nil(t, mgr.baseline.ByPath["cleanup.txt"], "entry still exists after cleanup")
}

func TestCommit_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	// Create original entry.
	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "old/path.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 100, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, create)

	// Move to new path.
	move := []Outcome{{
		Action: ActionLocalMove, Success: true,
		Path: "new/path.txt", OldPath: "old/path.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 100, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, move)

	assert.Nil(t, mgr.baseline.ByPath["old/path.txt"], "old path still exists after move")

	entry := mgr.baseline.ByPath["new/path.txt"]
	require.NotNil(t, entry, "new path not found after move")
	assert.Equal(t, "i", entry.ItemID)
}

func TestCommit_Conflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "conflict.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify conflict row was inserted with a valid UUID.
	var id, conflictPath, conflictType, resolution string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT id, path, conflict_type, resolution FROM conflicts LIMIT 1",
	).Scan(&id, &conflictPath, &conflictType, &resolution)
	require.NoError(t, err)

	_, uuidErr := uuid.Parse(id)
	assert.NoError(t, uuidErr, "conflict id = %q is not a valid UUID", id)
	assert.Equal(t, "conflict.txt", conflictPath)
	assert.Equal(t, "edit_edit", conflictType)
	assert.Equal(t, "unresolved", resolution)
}

func TestCommit_Conflict_StoresRemoteMtime(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	remoteMtime := int64(1700000000000000000) // non-zero nanosecond timestamp
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "mtime-test.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		Mtime:        1600000000000000000,
		RemoteMtime:  remoteMtime,
		ConflictType: "edit_edit",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify remote_mtime is stored as non-zero.
	var storedRemoteMtime sql.NullInt64

	err := mgr.db.QueryRowContext(ctx,
		"SELECT remote_mtime FROM conflicts WHERE path = ?", "mtime-test.txt",
	).Scan(&storedRemoteMtime)
	require.NoError(t, err)
	assert.True(t, storedRemoteMtime.Valid, "remote_mtime should be valid")
	assert.Equal(t, remoteMtime, storedRemoteMtime.Int64)
}

func TestCommit_SkipsFailedOutcomes(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action:  ActionDownload,
		Success: false, // should be skipped
		Path:    "should-not-exist.txt",
		DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 100,
	}}

	commitAll(t, mgr, ctx, outcomes)

	b, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, b.ByPath)
}

func TestCommit_DeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Commit with a delta token.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-abc", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-abc", token)
}

func TestCommit_DeltaTokenUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	// First commit with token.
	commitAll(t, mgr, ctx, outcomes)
	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-1", "d", "", "d"))

	// Second commit updates token.
	outcomes[0].LocalHash = "h2"
	outcomes[0].RemoteHash = "h2"

	commitAll(t, mgr, ctx, outcomes)
	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-2", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
}

func TestCommit_SyncedAtFromNowFunc(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Use a distinctive fixed time to verify nowFunc is used.
	fixedTime := time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 999,
	}}

	commitAll(t, mgr, ctx, outcomes)

	entry := mgr.baseline.ByPath["f.txt"]
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
}

func TestCommit_RefreshesCache(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Verify baseline is nil before first commit.
	assert.Nil(t, mgr.baseline, "baseline should be nil before first Load/Commit")

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	require.NotNil(t, mgr.baseline, "baseline should be populated after Commit")
	assert.Len(t, mgr.baseline.ByPath, 1)
}

func TestGetDeltaToken_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	token, err := mgr.GetDeltaToken(ctx, "nonexistent-drive", "")
	require.NoError(t, err)
	assert.Empty(t, token)
}

func TestGetDeltaToken_AfterCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("mydrv"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)
	require.NoError(t, mgr.CommitDeltaToken(ctx, "saved-token", "mydrv", "", "mydrv"))

	token, err := mgr.GetDeltaToken(ctx, "mydrv", "")
	require.NoError(t, err)
	assert.Equal(t, "saved-token", token)
}

func TestLoad_NullableFields(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Insert a row with NULL parent_id, hashes, size, mtime, etag directly.
	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, size, mtime, synced_at, etag)
		 VALUES (?, ?, ?, NULL, ?, NULL, NULL, NULL, NULL, ?, NULL)`,
		"root", "d", "root-id", "root", time.Now().UnixNano(),
	)
	require.NoError(t, err)

	b, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry := b.ByPath["root"]
	require.NotNil(t, entry, "root entry not found")
	assert.Empty(t, entry.ParentID)
	assert.Empty(t, entry.LocalHash)
	assert.Empty(t, entry.RemoteHash)
	assert.Zero(t, entry.Size)
	assert.Zero(t, entry.Mtime)
	assert.Empty(t, entry.ETag)
}

// ---------------------------------------------------------------------------
// Conflict query + resolve tests
// ---------------------------------------------------------------------------

// seedConflict inserts a conflict via CommitOutcome and returns its UUID.
func seedConflict(t *testing.T, mgr *SyncStore, path, conflictType string) string {
	t.Helper()

	ctx := t.Context()

	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         path,
		DriveID:      driveid.New("d"),
		ItemID:       "item-" + path,
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: conflictType,
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Retrieve the UUID.
	var id string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT id FROM conflicts WHERE path = ? ORDER BY detected_at DESC LIMIT 1", path,
	).Scan(&id)
	require.NoError(t, err, "retrieving conflict ID for %s", path)

	return id
}

func TestListConflicts_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	seedConflict(t, mgr, "a.txt", "edit_edit")
	seedConflict(t, mgr, "b.txt", "edit_delete")

	ctx := t.Context()

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 2)
	assert.Equal(t, "a.txt", conflicts[0].Path)
	assert.Equal(t, "b.txt", conflicts[1].Path)
}

func TestListConflicts_OnlyUnresolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "resolved.txt", "edit_edit")
	seedConflict(t, mgr, "pending.txt", "edit_edit")

	ctx := t.Context()

	// Resolve the first conflict.
	require.NoError(t, mgr.ResolveConflict(ctx, id, ResolutionKeepBoth))

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "pending.txt", conflicts[0].Path)
}

func TestGetConflict_ByID(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "lookup.txt", "create_create")
	ctx := t.Context()

	c, err := mgr.GetConflict(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, id, c.ID)
	assert.Equal(t, "lookup.txt", c.Path)
	assert.Equal(t, "create_create", c.ConflictType)
}

func TestGetConflict_ByPath(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	seedConflict(t, mgr, "bypath.txt", "edit_edit")
	ctx := t.Context()

	c, err := mgr.GetConflict(ctx, "bypath.txt")
	require.NoError(t, err)
	assert.Equal(t, "bypath.txt", c.Path)
}

func TestGetConflict_NotFound(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	_, err := mgr.GetConflict(ctx, "nonexistent")
	require.Error(t, err)
}

func TestResolveConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "resolve-me.txt", "edit_edit")
	ctx := t.Context()

	require.NoError(t, mgr.ResolveConflict(ctx, id, ResolutionKeepLocal))

	// Verify resolution was recorded.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.db.QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE id = ?", id,
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	require.NoError(t, err)
	assert.Equal(t, ResolutionKeepLocal, resolution)
	assert.Equal(t, "user", resolvedBy)
	assert.NotZero(t, resolvedAt)
}

func TestResolveConflict_AlreadyResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "double-resolve.txt", "edit_edit")
	ctx := t.Context()

	// First resolve succeeds.
	require.NoError(t, mgr.ResolveConflict(ctx, id, ResolutionKeepBoth))

	// Second resolve fails (already resolved).
	err := mgr.ResolveConflict(ctx, id, ResolutionKeepLocal)
	require.Error(t, err)
}

func TestLoad_ReturnsCachedBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed a baseline entry.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	// First Load returns the cached baseline from Commit's refresh.
	b1, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Second Load should return the same pointer (cached, no DB query).
	b2, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Same(t, b1, b2, "second Load returned a different *Baseline; expected cached pointer")
	assert.Len(t, b2.ByPath, 1)
}

func TestLoad_CacheInvalidatedByCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed one entry.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "first.txt", DriveID: driveid.New("d"), ItemID: "i1", ItemType: ItemTypeFile,
		LocalHash: "h1", RemoteHash: "h1", Size: 10, Mtime: 1,
	}}

	commitAll(t, mgr, ctx, outcomes)

	b1, err := mgr.Load(ctx)
	require.NoError(t, err)
	require.Len(t, b1.ByPath, 1)

	// Commit a second entry — cache should be invalidated and refreshed.
	outcomes2 := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "second.txt", DriveID: driveid.New("d"), ItemID: "i2", ItemType: ItemTypeFile,
		LocalHash: "h2", RemoteHash: "h2", Size: 20, Mtime: 2,
	}}

	commitAll(t, mgr, ctx, outcomes2)

	b2, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Len(t, b2.ByPath, 2, "cache should reflect both commits")
}

func TestMigrations_Idempotent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	// First open: runs migrations.
	mgr1, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err)
	mgr1.Close()

	// Second open: migrations should be a no-op.
	mgr2, err := NewSyncStore(dbPath, logger)
	require.NoError(t, err)
	defer mgr2.Close()

	// Verify the DB is still functional.
	ctx := t.Context()

	b, err := mgr2.Load(ctx)
	require.NoError(t, err)
	assert.NotNil(t, b)
}

func TestCommitConflict_AutoResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "auto-resolved.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "new-item",
		ParentID:     "root",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		Size:         512,
		Mtime:        fixedTime.UnixNano(),
		ConflictType: "edit_delete",
		ResolvedBy:   "auto",
	}}

	commitAll(t, mgr, ctx, outcomes)

	// Verify conflict row was inserted as already resolved.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.db.QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE path = ?",
		"auto-resolved.txt",
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	require.NoError(t, err)
	assert.Equal(t, ResolutionKeepLocal, resolution)
	assert.Equal(t, "auto", resolvedBy)
	assert.NotZero(t, resolvedAt)

	// Verify baseline was also updated (auto-resolve upserts baseline).
	entry := mgr.baseline.ByPath["auto-resolved.txt"]
	require.NotNil(t, entry, "baseline entry not found for auto-resolved conflict")
	assert.Equal(t, "new-item", entry.ItemID)
	assert.Equal(t, "local-h", entry.LocalHash)
}

func TestListAllConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed an unresolved conflict.
	seedConflict(t, mgr, "unresolved.txt", "edit_edit")

	// Seed and resolve a conflict.
	resolvedID := seedConflict(t, mgr, "resolved-file.txt", "edit_delete")
	ctx := t.Context()

	require.NoError(t, mgr.ResolveConflict(ctx, resolvedID, ResolutionKeepLocal))

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
			assert.Equal(t, ResolutionKeepLocal, all[i].Resolution)
			assert.Equal(t, "user", all[i].ResolvedBy)
			assert.NotZero(t, all[i].ResolvedAt)
		}
	}

	assert.True(t, found, "resolved-file.txt not found in ListAllConflicts results")
}

// ---------------------------------------------------------------------------
// CommitOutcome tests
// ---------------------------------------------------------------------------

func TestCommitOutcome_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	fixedTime := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcome := Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       "co-download.txt",
		DriveID:    driveid.New("d"),
		ItemID:     "i1",
		ParentID:   "p1",
		ItemType:   ItemTypeFile,
		LocalHash:  "lh",
		RemoteHash: "rh",
		Size:       512,
		Mtime:      fixedTime.UnixNano(),
		ETag:       "etag1",
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.baseline.GetByPath("co-download.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i1", entry.ItemID)
	assert.Equal(t, "lh", entry.LocalHash)
	assert.Equal(t, fixedTime.UnixNano(), entry.SyncedAt)
}

func TestCommitOutcome_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	outcome := Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       "co-upload.txt",
		DriveID:    driveid.New("d"),
		ItemID:     "i2",
		ItemType:   ItemTypeFile,
		LocalHash:  "h",
		RemoteHash: "h",
		Size:       256,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.baseline.GetByPath("co-upload.txt")
	require.True(t, ok, "baseline entry not found")
	assert.Equal(t, "i2", entry.ItemID)
}

func TestCommitOutcome_Delete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	// Seed an entry first.
	seed := Outcome{
		Action: ActionDownload, Success: true,
		Path: "co-delete.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h", Size: 10,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &seed))

	del := Outcome{Action: ActionLocalDelete, Success: true, Path: "co-delete.txt"}
	require.NoError(t, mgr.CommitOutcome(ctx, &del))

	_, ok := mgr.baseline.GetByPath("co-delete.txt")
	assert.False(t, ok, "entry still exists after delete")
}

func TestCommitOutcome_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	// Seed original entry.
	seed := Outcome{
		Action: ActionDownload, Success: true,
		Path: "old/move.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p1",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h", Size: 10,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &seed))

	move := Outcome{
		Action: ActionLocalMove, Success: true,
		Path: "new/move.txt", OldPath: "old/move.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h", Size: 10,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &move))

	_, ok := mgr.baseline.GetByPath("old/move.txt")
	assert.False(t, ok, "old path still exists after move")

	entry, ok := mgr.baseline.GetByPath("new/move.txt")
	require.True(t, ok, "new path not found after move")
	assert.Equal(t, "p2", entry.ParentID)
}

func TestCommitOutcome_Conflict_AutoResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	outcome := Outcome{
		Action:       ActionConflict,
		Success:      true,
		Path:         "co-conflict.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "new-item",
		ItemType:     ItemTypeFile,
		LocalHash:    "lh",
		RemoteHash:   "rh",
		ConflictType: ConflictEditEdit,
		ResolvedBy:   ResolvedByAuto,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	// Auto-resolved conflict should update baseline.
	entry, ok := mgr.baseline.GetByPath("co-conflict.txt")
	require.True(t, ok, "baseline entry not found for auto-resolved conflict")
	assert.Equal(t, "new-item", entry.ItemID)

	// Conflict row should exist.
	var resolution string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT resolution FROM conflicts WHERE path = ?", "co-conflict.txt",
	).Scan(&resolution)
	require.NoError(t, err)
	assert.Equal(t, ResolutionKeepLocal, resolution)
}

func TestCommitOutcome_Conflict_Unresolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	outcome := Outcome{
		Action:       ActionConflict,
		Success:      true,
		Path:         "co-unresolved.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i",
		ItemType:     ItemTypeFile,
		ConflictType: ConflictEditEdit,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	// Unresolved conflict should NOT update baseline.
	_, ok := mgr.baseline.GetByPath("co-unresolved.txt")
	assert.False(t, ok, "baseline entry should not exist for unresolved conflict")
}

func TestCommitOutcome_EditDeleteConflict_DeletesBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	// First, create a baseline entry for the file.
	setupOutcome := Outcome{
		Action:   ActionDownload,
		Success:  true,
		Path:     "edit-delete-target.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "i1",
		ItemType: ItemTypeFile,
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &setupOutcome))

	// Verify baseline entry exists.
	_, ok := mgr.baseline.GetByPath("edit-delete-target.txt")
	require.True(t, ok, "baseline entry should exist after download")

	// Now commit an unresolved edit-delete conflict (B-133).
	conflictOutcome := Outcome{
		Action:       ActionConflict,
		Success:      true,
		Path:         "edit-delete-target.txt",
		DriveID:      driveid.New("d"),
		ItemID:       "i1",
		ItemType:     ItemTypeFile,
		ConflictType: ConflictEditDelete,
		LocalHash:    "modified-hash",
		RemoteHash:   "baseline-remote-hash",
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &conflictOutcome))

	// Baseline entry should be deleted — the original file was renamed to conflict copy.
	_, ok = mgr.baseline.GetByPath("edit-delete-target.txt")
	assert.False(t, ok, "baseline entry should be deleted for unresolved edit-delete conflict")

	// Conflict record should exist.
	var conflictType, resolution string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT conflict_type, resolution FROM conflicts WHERE path = ?", "edit-delete-target.txt",
	).Scan(&conflictType, &resolution)
	require.NoError(t, err)
	assert.Equal(t, ConflictEditDelete, conflictType)
	assert.Equal(t, ResolutionUnresolved, resolution)
}

func TestCommitOutcome_SkipsFailedOutcome(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	outcome := Outcome{
		Action:  ActionDownload,
		Success: false,
		Path:    "should-not-exist.txt",
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	if mgr.baseline != nil {
		_, ok := mgr.baseline.GetByPath("should-not-exist.txt")
		assert.False(t, ok, "failed outcome should not create baseline entry")
	}
}

func TestCommitOutcome_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	outcome := Outcome{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     "Documents/Reports",
		DriveID:  driveid.New("d"),
		ItemID:   "folder-id",
		ParentID: "root",
		ItemType: ItemTypeFolder,
	}

	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	entry, ok := mgr.baseline.GetByPath("Documents/Reports")
	require.True(t, ok, "folder entry not found")
	assert.Equal(t, ItemTypeFolder, entry.ItemType)
	assert.Equal(t, "folder-id", entry.ItemID)
}

// ---------------------------------------------------------------------------
// CommitDeltaToken tests
// ---------------------------------------------------------------------------

func TestCommitDeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-abc", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-abc", token)
}

func TestCommitDeltaToken_Update(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	mgr.nowFunc = func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) }

	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-1", "d", "", "d"))
	require.NoError(t, mgr.CommitDeltaToken(ctx, "token-2", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
}

func TestCommitDeltaToken_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Empty token should be a no-op.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "", "d", "", "d"))

	token, err := mgr.GetDeltaToken(ctx, "d", "")
	require.NoError(t, err)
	assert.Empty(t, token)
}

func TestCommitDeltaToken_CompositeKey_DifferentScopes(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
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

func TestCommitDeltaToken_CompositeKey_UpdatePreservesOtherScopes(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
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

func TestBaseline_GetByPath(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs/readme.md": {Path: "docs/readme.md", ItemID: "item1", DriveID: driveid.New("d1")},
		},
		ByID: make(map[driveid.ItemKey]*BaselineEntry),
	}

	entry, ok := b.GetByPath("docs/readme.md")
	require.True(t, ok)
	assert.Equal(t, "item1", entry.ItemID)

	_, ok = b.GetByPath("nonexistent")
	assert.False(t, ok)
}

func TestBaseline_GetByID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	key := driveid.NewItemKey(driveID, "item1")
	entry := &BaselineEntry{Path: "test.txt", ItemID: "item1", DriveID: driveID}

	b := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   map[driveid.ItemKey]*BaselineEntry{key: entry},
	}

	got, ok := b.GetByID(key)
	require.True(t, ok)
	assert.Equal(t, "test.txt", got.Path)

	missingKey := driveid.NewItemKey(driveID, "nonexistent")
	_, ok = b.GetByID(missingKey)
	assert.False(t, ok)
}

func TestBaseline_Put(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   make(map[driveid.ItemKey]*BaselineEntry),
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

	key := driveid.NewItemKey(driveid.New("d1"), "item-new")
	gotByID, ok := b.GetByID(key)
	require.True(t, ok, "entry not found by ID after Put")
	assert.Equal(t, "new/file.txt", gotByID.Path)
}

func TestBaseline_Delete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("d1")
	entry := &BaselineEntry{Path: "delete-me.txt", DriveID: driveID, ItemID: "item-del"}
	key := driveid.NewItemKey(driveID, "item-del")

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{"delete-me.txt": entry},
		ByID:   map[driveid.ItemKey]*BaselineEntry{key: entry},
	}

	b.Delete("delete-me.txt")

	_, ok := b.GetByPath("delete-me.txt")
	assert.False(t, ok, "entry still exists after Delete")

	_, ok = b.GetByID(key)
	assert.False(t, ok, "entry still exists in ByID after Delete")

	// Deleting nonexistent path should not panic.
	b.Delete("nonexistent")
}

func TestBaseline_Len(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"a.txt": {Path: "a.txt"},
			"b.txt": {Path: "b.txt"},
		},
		ByID: make(map[driveid.ItemKey]*BaselineEntry),
	}

	assert.Equal(t, 2, b.Len())
}

func TestBaseline_ForEachPath(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"a.txt": {Path: "a.txt", ItemID: "i1"},
			"b.txt": {Path: "b.txt", ItemID: "i2"},
		},
		ByID: make(map[driveid.ItemKey]*BaselineEntry),
	}

	paths := make(map[string]bool)
	b.ForEachPath(func(path string, entry *BaselineEntry) {
		paths[path] = true
	})

	assert.Len(t, paths, 2)
	assert.True(t, paths["a.txt"], "ForEachPath did not visit a.txt")
	assert.True(t, paths["b.txt"], "ForEachPath did not visit b.txt")
}

func TestBaseline_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   make(map[driveid.ItemKey]*BaselineEntry),
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

// TestConflictRecord_NameField verifies that ConflictRecord.Name is populated
// as path.Base(Path) by the shared scanConflict function (B-071).
func TestConflictRecord_NameField(t *testing.T) {
	mgr := newTestManager(t)
	ctx := t.Context()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a conflict with a nested path.
	outcome := &Outcome{
		Action:       ActionConflict,
		Success:      true,
		Path:         "docs/notes/readme.md",
		DriveID:      driveid.New(testDriveID),
		ItemID:       "item-name-test",
		ConflictType: ConflictEditEdit,
		LocalHash:    "localH",
		RemoteHash:   "remoteH",
		Mtime:        100,
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

	mgr.nowFunc = func() time.Time { return now.AddDate(0, 0, -120) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionConflict, Success: true,
		Path: "old-resolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-old", ConflictType: ConflictEditEdit,
		LocalHash: "lh1", RemoteHash: "rh1", Mtime: 100, RemoteMtime: 200,
	}))

	mgr.nowFunc = func() time.Time { return now.AddDate(0, 0, -100) }

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)

	oldID = conflicts[0].ID
	require.NoError(t, mgr.ResolveConflict(ctx, oldID, "keep_local"))

	mgr.nowFunc = func() time.Time { return now.AddDate(0, 0, -10) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionConflict, Success: true,
		Path: "new-resolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-new", ConflictType: ConflictEditEdit,
		LocalHash: "lh2", RemoteHash: "rh2", Mtime: 300, RemoteMtime: 400,
	}))

	mgr.nowFunc = func() time.Time { return now.AddDate(0, 0, -5) }

	conflicts, err = mgr.ListConflicts(ctx)
	require.NoError(t, err)

	for _, c := range conflicts {
		if c.Path == "new-resolved.txt" {
			newID = c.ID
		}
	}

	require.NoError(t, mgr.ResolveConflict(ctx, newID, "keep_remote"))

	mgr.nowFunc = func() time.Time { return now.AddDate(0, 0, -120) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionConflict, Success: true,
		Path: "unresolved.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-unresolved", ConflictType: ConflictEditEdit,
		LocalHash: "lh3", RemoteHash: "rh3", Mtime: 500, RemoteMtime: 600,
	}))

	return oldID, newID
}

// TestPruneResolvedConflicts verifies that PruneResolvedConflicts deletes
// resolved conflicts older than the retention period while preserving
// newer resolved and all unresolved conflicts (B-087).
func TestPruneResolvedConflicts(t *testing.T) {
	mgr := newTestManager(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	mgr.nowFunc = func() time.Time { return now }

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	oldID, newID := setupPruneTestConflicts(t, mgr, ctx, now)

	mgr.nowFunc = func() time.Time { return now }

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

// TestCheckCacheConsistency verifies that CheckCacheConsistency detects
// mismatches between the in-memory baseline cache and the database (B-198).
func TestCheckCacheConsistency(t *testing.T) {
	mgr := newTestManager(t)
	ctx := t.Context()

	_, err := mgr.Load(ctx)
	require.NoError(t, err)

	// Insert a baseline entry via CommitOutcome.
	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true,
		Path: "consistency-check.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item-cc", ParentID: "root", ItemType: ItemTypeFile,
		LocalHash: "hash1", RemoteHash: "hash1",
		Size: 100, Mtime: 1000,
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

func TestConsolidatedSchema_AllTablesCreated(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Verify all expected tables exist by querying sqlite_master.
	expectedTables := []string{
		"baseline", "delta_tokens", "conflicts", "sync_metadata",
		"remote_state", "local_issues",
	}

	for _, table := range expectedTables {
		var name string
		err := mgr.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		require.NoError(t, err, "table %q should exist", table)
		assert.Equal(t, table, name)
	}

	// Verify delta_tokens composite key: two tokens for same drive_id,
	// different scope_id should coexist.
	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, token, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"d!abc123", "", "d!abc123", "primary-token", 1700000000)
	require.NoError(t, err)

	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, token, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"d!abc123", "shared-folder-id", "d!other456", "scoped-token", 1700000001)
	require.NoError(t, err)

	var count int
	err = mgr.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM delta_tokens WHERE drive_id = ?`, "d!abc123",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Verify remote_state table structure: insert + query.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item1", "/test.txt", "file", "pending_download", 1700000000)
	require.NoError(t, err)

	// Verify local_issues table structure: insert + query.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO local_issues (path, issue_type, sync_status, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"/bad-file.txt", "invalid_filename", "pending_upload", 1700000000, 1700000000)
	require.NoError(t, err)

	// Verify remote_state CHECK constraint rejects invalid status.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item2", "/bad.txt", "file", "invalid_status", 1700000000)
	require.Error(t, err, "invalid sync_status should be rejected by CHECK constraint")
}

func TestConsolidatedSchema_RemoteStateActivePathUnique(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Insert an active item at a path.
	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item1", "/test.txt", "file", "synced", 1700000000)
	require.NoError(t, err)

	// Another active item at the same path should be rejected by the partial unique index.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item2", "/test.txt", "file", "pending_download", 1700000000)
	require.Error(t, err, "duplicate active path should be rejected")

	// A deleted item at the same path should be allowed.
	_, err = mgr.db.ExecContext(ctx,
		`INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d!abc123", "item3", "/test.txt", "file", "deleted", 1700000000)
	require.NoError(t, err, "deleted item at same path should be allowed")
}

// --- Sync metadata tests (6.2b) ---

func TestWriteSyncMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	report := &SyncReport{
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

func TestWriteSyncMetadata_Upsert(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	report1 := &SyncReport{Duration: 1 * time.Second, Succeeded: 10}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report1))

	report2 := &SyncReport{Duration: 2 * time.Second, Succeeded: 20}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report2))

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, "20", meta["last_sync_succeeded"], "should be from second write")
	assert.Equal(t, "2000", meta["last_sync_duration_ms"])
}

func TestWriteSyncMetadata_NoErrors(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	report := &SyncReport{Duration: 500 * time.Millisecond, Succeeded: 5}
	require.NoError(t, mgr.WriteSyncMetadata(ctx, report))

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Empty(t, meta["last_sync_error"])
}

func TestReadSyncMetadata_EmptyDB(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	meta, err := mgr.ReadSyncMetadata(ctx)
	require.NoError(t, err)
	assert.Empty(t, meta)
}

func TestBaselineEntryCount_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	count, err := mgr.BaselineEntryCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestBaselineEntryCount_WithEntries(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Commit a download outcome to add a baseline entry.
	outcome := Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       "/test/file.txt",
		DriveID:    driveid.New("d!123"),
		ItemID:     "item-1",
		ParentID:   "parent-1",
		ItemType:   ItemTypeFile,
		RemoteHash: "hash123",
		Size:       1024,
		Mtime:      time.Now().UnixNano(),
	}
	require.NoError(t, mgr.CommitOutcome(ctx, &outcome))

	count, err := mgr.BaselineEntryCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestUnresolvedConflictCount_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	count, err := mgr.UnresolvedConflictCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
