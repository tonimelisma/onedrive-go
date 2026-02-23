package sync

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// testLogger returns a debug-level logger that writes to t.Log,
// so all activity appears in CI output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(&testLogWriter{t: t}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// testLogWriter adapts testing.T to io.Writer for slog.
type testLogWriter struct {
	t *testing.T
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))

	return len(p), nil
}

// newTestManager creates a BaselineManager backed by a temp directory,
// registering cleanup with t.Cleanup.
func newTestManager(t *testing.T) *BaselineManager {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	mgr, err := NewBaselineManager(dbPath, logger)
	if err != nil {
		t.Fatalf("NewBaselineManager(%q): %v", dbPath, err)
	}

	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	return mgr
}

func TestNewBaselineManager_CreatesDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	mgr, err := NewBaselineManager(dbPath, logger)
	if err != nil {
		t.Fatalf("NewBaselineManager: %v", err)
	}
	defer mgr.Close()

	// Verify DB file exists by opening a direct connection.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNewBaselineManager_WALMode(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)

	var journalMode string

	ctx := context.Background()
	err := mgr.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}

	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q", journalMode, "wal")
	}
}

func TestNewBaselineManager_RunsMigrations(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)

	// goose creates a goose_db_version table automatically.
	ctx := context.Background()

	var count int

	err := mgr.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0",
	).Scan(&count)
	if err != nil {
		t.Fatalf("querying goose_db_version: %v", err)
	}

	if count == 0 {
		t.Error("no migrations applied (goose_db_version has no entries)")
	}
}

func TestLoad_EmptyBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	b, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(b.ByPath) != 0 {
		t.Errorf("ByPath has %d entries, want 0", len(b.ByPath))
	}

	if len(b.ByID) != 0 {
		t.Errorf("ByID has %d entries, want 0", len(b.ByID))
	}
}

func TestCommit_Download(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	err := mgr.Commit(ctx, outcomes, "", "drive1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entry := mgr.baseline.ByPath["docs/readme.md"]
	if entry == nil {
		t.Fatal("baseline entry not found for docs/readme.md")
	}

	if !entry.DriveID.Equal(driveid.New("drive1")) {
		t.Errorf("DriveID = %q, want %q", entry.DriveID, driveid.New("drive1"))
	}

	if entry.ItemID != "item1" {
		t.Errorf("ItemID = %q, want %q", entry.ItemID, "item1")
	}

	if entry.LocalHash != "abc123" {
		t.Errorf("LocalHash = %q, want %q", entry.LocalHash, "abc123")
	}

	if entry.SyncedAt != fixedTime.UnixNano() {
		t.Errorf("SyncedAt = %d, want %d", entry.SyncedAt, fixedTime.UnixNano())
	}
}

func TestCommit_Upload(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	err := mgr.Commit(ctx, outcomes, "", "drive2")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entry := mgr.baseline.ByPath["photos/cat.jpg"]
	if entry == nil {
		t.Fatal("baseline entry not found")
	}

	if entry.LocalHash != "hash-local" {
		t.Errorf("LocalHash = %q, want %q", entry.LocalHash, "hash-local")
	}

	if entry.RemoteHash != "hash-remote" {
		t.Errorf("RemoteHash = %q, want %q", entry.RemoteHash, "hash-remote")
	}
}

func TestCommit_FolderCreate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	err := mgr.Commit(ctx, outcomes, "", "drive1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entry := mgr.baseline.ByPath["Documents/Reports"]
	if entry == nil {
		t.Fatal("folder entry not found")
	}

	if entry.ItemType != ItemTypeFolder {
		t.Errorf("ItemType = %v, want Folder", entry.ItemType)
	}

	// Folders have no hash or size.
	if entry.LocalHash != "" {
		t.Errorf("LocalHash = %q, want empty", entry.LocalHash)
	}

	if entry.Size != 0 {
		t.Errorf("Size = %d, want 0", entry.Size)
	}
}

func TestCommit_UpdateSynced(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	// Second commit: convergent edit updates synced_at.
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return t2 }

	outcomes[0].Action = ActionUpdateSynced
	outcomes[0].LocalHash = "h2"
	outcomes[0].RemoteHash = "h2"

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	entry := mgr.baseline.ByPath["file.txt"]
	if entry.SyncedAt != t2.UnixNano() {
		t.Errorf("SyncedAt = %d, want %d", entry.SyncedAt, t2.UnixNano())
	}

	if entry.LocalHash != "h2" {
		t.Errorf("LocalHash = %q, want %q", entry.LocalHash, "h2")
	}
}

func TestCommit_LocalDelete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	if err := mgr.Commit(ctx, create, "", "d"); err != nil {
		t.Fatalf("create Commit: %v", err)
	}

	del := []Outcome{{
		Action: ActionLocalDelete, Success: true, Path: "delete-me.txt",
	}}

	if err := mgr.Commit(ctx, del, "", "d"); err != nil {
		t.Fatalf("delete Commit: %v", err)
	}

	if mgr.baseline.ByPath["delete-me.txt"] != nil {
		t.Error("entry still exists after local delete")
	}
}

func TestCommit_RemoteDelete(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "remote-del.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 50, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, create, "", "d"); err != nil {
		t.Fatalf("create Commit: %v", err)
	}

	del := []Outcome{{
		Action: ActionRemoteDelete, Success: true, Path: "remote-del.txt",
	}}

	if err := mgr.Commit(ctx, del, "", "d"); err != nil {
		t.Fatalf("delete Commit: %v", err)
	}

	if mgr.baseline.ByPath["remote-del.txt"] != nil {
		t.Error("entry still exists after remote delete")
	}
}

func TestCommit_Cleanup(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "cleanup.txt", DriveID: driveid.New("d"), ItemID: "i",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 50, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, create, "", "d"); err != nil {
		t.Fatalf("create Commit: %v", err)
	}

	cleanup := []Outcome{{
		Action: ActionCleanup, Success: true, Path: "cleanup.txt",
	}}

	if err := mgr.Commit(ctx, cleanup, "", "d"); err != nil {
		t.Fatalf("cleanup Commit: %v", err)
	}

	if mgr.baseline.ByPath["cleanup.txt"] != nil {
		t.Error("entry still exists after cleanup")
	}
}

func TestCommit_Move(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	// Create original entry.
	create := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "old/path.txt", DriveID: driveid.New("d"), ItemID: "i", ParentID: "p",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 100, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, create, "", "d"); err != nil {
		t.Fatalf("create Commit: %v", err)
	}

	// Move to new path.
	move := []Outcome{{
		Action: ActionLocalMove, Success: true,
		Path: "new/path.txt", OldPath: "old/path.txt",
		DriveID: driveid.New("d"), ItemID: "i", ParentID: "p2",
		ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		Size: 100, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, move, "", "d"); err != nil {
		t.Fatalf("move Commit: %v", err)
	}

	if mgr.baseline.ByPath["old/path.txt"] != nil {
		t.Error("old path still exists after move")
	}

	entry := mgr.baseline.ByPath["new/path.txt"]
	if entry == nil {
		t.Fatal("new path not found after move")
	}

	if entry.ItemID != "i" {
		t.Errorf("ItemID = %q, want %q", entry.ItemID, "i")
	}
}

func TestCommit_Conflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify conflict row was inserted with a valid UUID.
	var id, conflictPath, conflictType, resolution string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT id, path, conflict_type, resolution FROM conflicts LIMIT 1",
	).Scan(&id, &conflictPath, &conflictType, &resolution)
	if err != nil {
		t.Fatalf("querying conflicts: %v", err)
	}

	if _, uuidErr := uuid.Parse(id); uuidErr != nil {
		t.Errorf("conflict id = %q, not a valid UUID: %v", id, uuidErr)
	}

	if conflictPath != "conflict.txt" {
		t.Errorf("path = %q, want %q", conflictPath, "conflict.txt")
	}

	if conflictType != "edit_edit" {
		t.Errorf("conflict_type = %q, want %q", conflictType, "edit_edit")
	}

	if resolution != "unresolved" {
		t.Errorf("resolution = %q, want %q", resolution, "unresolved")
	}
}

func TestCommit_SkipsFailedOutcomes(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	b, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(b.ByPath) != 0 {
		t.Errorf("expected empty baseline, got %d entries", len(b.ByPath))
	}
}

func TestCommit_DeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Commit with a delta token.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, outcomes, "token-abc", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	token, err := mgr.GetDeltaToken(ctx, "d")
	if err != nil {
		t.Fatalf("GetDeltaToken: %v", err)
	}

	if token != "token-abc" {
		t.Errorf("token = %q, want %q", token, "token-abc")
	}
}

func TestCommit_DeltaTokenUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	// First commit with token.
	if err := mgr.Commit(ctx, outcomes, "token-1", "d"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	// Second commit updates token.
	outcomes[0].LocalHash = "h2"
	outcomes[0].RemoteHash = "h2"

	if err := mgr.Commit(ctx, outcomes, "token-2", "d"); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	token, err := mgr.GetDeltaToken(ctx, "d")
	if err != nil {
		t.Fatalf("GetDeltaToken: %v", err)
	}

	if token != "token-2" {
		t.Errorf("token = %q, want %q", token, "token-2")
	}
}

func TestCommit_SyncedAtFromNowFunc(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	// Use a distinctive fixed time to verify nowFunc is used.
	fixedTime := time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return fixedTime }

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 999,
	}}

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entry := mgr.baseline.ByPath["f.txt"]
	if entry.SyncedAt != fixedTime.UnixNano() {
		t.Errorf("SyncedAt = %d, want %d (from nowFunc)", entry.SyncedAt, fixedTime.UnixNano())
	}
}

func TestCommit_RefreshesCache(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Verify baseline is nil before first commit.
	if mgr.baseline != nil {
		t.Error("baseline should be nil before first Load/Commit")
	}

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if mgr.baseline == nil {
		t.Fatal("baseline should be populated after Commit")
	}

	if len(mgr.baseline.ByPath) != 1 {
		t.Errorf("ByPath has %d entries, want 1", len(mgr.baseline.ByPath))
	}
}

func TestGetDeltaToken_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	token, err := mgr.GetDeltaToken(ctx, "nonexistent-drive")
	if err != nil {
		t.Fatalf("GetDeltaToken: %v", err)
	}

	if token != "" {
		t.Errorf("token = %q, want empty string", token)
	}
}

func TestGetDeltaToken_AfterCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	mgr.nowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "f.txt", DriveID: driveid.New("mydrv"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, outcomes, "saved-token", "mydrv"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	token, err := mgr.GetDeltaToken(ctx, "mydrv")
	if err != nil {
		t.Fatalf("GetDeltaToken: %v", err)
	}

	if token != "saved-token" {
		t.Errorf("token = %q, want %q", token, "saved-token")
	}
}

func TestLoad_NullableFields(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	// Insert a row with NULL parent_id, hashes, size, mtime, etag directly.
	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, size, mtime, synced_at, etag)
		 VALUES (?, ?, ?, NULL, ?, NULL, NULL, NULL, NULL, ?, NULL)`,
		"root", "d", "root-id", "root", time.Now().UnixNano(),
	)
	if err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	b, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entry := b.ByPath["root"]
	if entry == nil {
		t.Fatal("root entry not found")
	}

	if entry.ParentID != "" {
		t.Errorf("ParentID = %q, want empty", entry.ParentID)
	}

	if entry.LocalHash != "" {
		t.Errorf("LocalHash = %q, want empty", entry.LocalHash)
	}

	if entry.RemoteHash != "" {
		t.Errorf("RemoteHash = %q, want empty", entry.RemoteHash)
	}

	if entry.Size != 0 {
		t.Errorf("Size = %d, want 0", entry.Size)
	}

	if entry.Mtime != 0 {
		t.Errorf("Mtime = %d, want 0", entry.Mtime)
	}

	if entry.ETag != "" {
		t.Errorf("ETag = %q, want empty", entry.ETag)
	}
}

func TestMigrations_Idempotent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	logger := testLogger(t)

	// First open: runs migrations.
	mgr1, err := NewBaselineManager(dbPath, logger)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	mgr1.Close()

	// Second open: migrations should be a no-op.
	mgr2, err := NewBaselineManager(dbPath, logger)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer mgr2.Close()

	// Verify the DB is still functional.
	ctx := context.Background()

	b, err := mgr2.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if b == nil {
		t.Error("Load returned nil baseline")
	}
}
