package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	stdsync "sync"
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

// ---------------------------------------------------------------------------
// Conflict query + resolve tests
// ---------------------------------------------------------------------------

// seedConflict inserts a conflict via Commit and returns its UUID.
func seedConflict(t *testing.T, mgr *BaselineManager, path, conflictType string) string {
	t.Helper()

	ctx := context.Background()

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

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("seedConflict(%s): %v", path, err)
	}

	// Retrieve the UUID.
	var id string

	err := mgr.db.QueryRowContext(ctx,
		"SELECT id FROM conflicts WHERE path = ? ORDER BY detected_at DESC LIMIT 1", path,
	).Scan(&id)
	if err != nil {
		t.Fatalf("retrieving conflict ID for %s: %v", path, err)
	}

	return id
}

func TestListConflicts_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}
}

func TestListConflicts_WithConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	seedConflict(t, mgr, "a.txt", "edit_edit")
	seedConflict(t, mgr, "b.txt", "edit_delete")

	ctx := context.Background()

	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	if conflicts[0].Path != "a.txt" {
		t.Errorf("first conflict path = %q, want %q", conflicts[0].Path, "a.txt")
	}

	if conflicts[1].Path != "b.txt" {
		t.Errorf("second conflict path = %q, want %q", conflicts[1].Path, "b.txt")
	}
}

func TestListConflicts_OnlyUnresolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "resolved.txt", "edit_edit")
	seedConflict(t, mgr, "pending.txt", "edit_edit")

	ctx := context.Background()

	// Resolve the first conflict.
	if err := mgr.ResolveConflict(ctx, id, ResolutionKeepBoth); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 unresolved conflict, got %d", len(conflicts))
	}

	if conflicts[0].Path != "pending.txt" {
		t.Errorf("conflict path = %q, want %q", conflicts[0].Path, "pending.txt")
	}
}

func TestGetConflict_ByID(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "lookup.txt", "create_create")
	ctx := context.Background()

	c, err := mgr.GetConflict(ctx, id)
	if err != nil {
		t.Fatalf("GetConflict by ID: %v", err)
	}

	if c.ID != id {
		t.Errorf("ID = %q, want %q", c.ID, id)
	}

	if c.Path != "lookup.txt" {
		t.Errorf("Path = %q, want %q", c.Path, "lookup.txt")
	}

	if c.ConflictType != "create_create" {
		t.Errorf("ConflictType = %q, want %q", c.ConflictType, "create_create")
	}
}

func TestGetConflict_ByPath(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	seedConflict(t, mgr, "bypath.txt", "edit_edit")
	ctx := context.Background()

	c, err := mgr.GetConflict(ctx, "bypath.txt")
	if err != nil {
		t.Fatalf("GetConflict by path: %v", err)
	}

	if c.Path != "bypath.txt" {
		t.Errorf("Path = %q, want %q", c.Path, "bypath.txt")
	}
}

func TestGetConflict_NotFound(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

	_, err := mgr.GetConflict(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent conflict, got nil")
	}
}

func TestResolveConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "resolve-me.txt", "edit_edit")
	ctx := context.Background()

	if err := mgr.ResolveConflict(ctx, id, ResolutionKeepLocal); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	// Verify resolution was recorded.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.db.QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE id = ?", id,
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	if err != nil {
		t.Fatalf("querying conflict: %v", err)
	}

	if resolution != ResolutionKeepLocal {
		t.Errorf("resolution = %q, want %q", resolution, ResolutionKeepLocal)
	}

	if resolvedBy != "user" {
		t.Errorf("resolved_by = %q, want %q", resolvedBy, "user")
	}

	if resolvedAt == 0 {
		t.Error("resolved_at should be non-zero")
	}
}

func TestResolveConflict_AlreadyResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	id := seedConflict(t, mgr, "double-resolve.txt", "edit_edit")
	ctx := context.Background()

	// First resolve succeeds.
	if err := mgr.ResolveConflict(ctx, id, ResolutionKeepBoth); err != nil {
		t.Fatalf("first ResolveConflict: %v", err)
	}

	// Second resolve fails (already resolved).
	err := mgr.ResolveConflict(ctx, id, ResolutionKeepLocal)
	if err == nil {
		t.Fatal("expected error for already-resolved conflict, got nil")
	}
}

func TestLoad_ReturnsCachedBaseline(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed a baseline entry.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i", ItemType: ItemTypeFile,
		LocalHash: "h", RemoteHash: "h", Size: 10, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// First Load returns the cached baseline from Commit's refresh.
	b1, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}

	// Second Load should return the same pointer (cached, no DB query).
	b2, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if b1 != b2 {
		t.Error("second Load returned a different *Baseline; expected cached pointer")
	}

	if len(b2.ByPath) != 1 {
		t.Errorf("ByPath has %d entries, want 1", len(b2.ByPath))
	}
}

func TestLoad_CacheInvalidatedByCommit(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed one entry.
	outcomes := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "first.txt", DriveID: driveid.New("d"), ItemID: "i1", ItemType: ItemTypeFile,
		LocalHash: "h1", RemoteHash: "h1", Size: 10, Mtime: 1,
	}}

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	b1, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load after first commit: %v", err)
	}

	if len(b1.ByPath) != 1 {
		t.Fatalf("ByPath has %d entries, want 1", len(b1.ByPath))
	}

	// Commit a second entry — cache should be invalidated and refreshed.
	outcomes2 := []Outcome{{
		Action: ActionDownload, Success: true,
		Path: "second.txt", DriveID: driveid.New("d"), ItemID: "i2", ItemType: ItemTypeFile,
		LocalHash: "h2", RemoteHash: "h2", Size: 20, Mtime: 2,
	}}

	if commitErr := mgr.Commit(ctx, outcomes2, "", "d"); commitErr != nil {
		t.Fatalf("second Commit: %v", commitErr)
	}

	b2, loadErr := mgr.Load(ctx)
	if loadErr != nil {
		t.Fatalf("Load after second commit: %v", loadErr)
	}

	if len(b2.ByPath) != 2 {
		t.Errorf("ByPath has %d entries, want 2 (cache should reflect both commits)", len(b2.ByPath))
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

func TestCommitConflict_AutoResolved(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := context.Background()

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

	if err := mgr.Commit(ctx, outcomes, "", "d"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify conflict row was inserted as already resolved.
	var resolution, resolvedBy string
	var resolvedAt int64

	err := mgr.db.QueryRowContext(ctx,
		"SELECT resolution, resolved_at, resolved_by FROM conflicts WHERE path = ?",
		"auto-resolved.txt",
	).Scan(&resolution, &resolvedAt, &resolvedBy)
	if err != nil {
		t.Fatalf("querying conflict: %v", err)
	}

	if resolution != ResolutionKeepLocal {
		t.Errorf("resolution = %q, want %q", resolution, ResolutionKeepLocal)
	}

	if resolvedBy != "auto" {
		t.Errorf("resolved_by = %q, want %q", resolvedBy, "auto")
	}

	if resolvedAt == 0 {
		t.Error("resolved_at should be non-zero")
	}

	// Verify baseline was also updated (auto-resolve upserts baseline).
	entry := mgr.baseline.ByPath["auto-resolved.txt"]
	if entry == nil {
		t.Fatal("baseline entry not found for auto-resolved conflict")
	}

	if entry.ItemID != "new-item" {
		t.Errorf("ItemID = %q, want %q", entry.ItemID, "new-item")
	}

	if entry.LocalHash != "local-h" {
		t.Errorf("LocalHash = %q, want %q", entry.LocalHash, "local-h")
	}
}

func TestListAllConflicts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	mgr.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Seed an unresolved conflict.
	seedConflict(t, mgr, "unresolved.txt", "edit_edit")

	// Seed and resolve a conflict.
	resolvedID := seedConflict(t, mgr, "resolved-file.txt", "edit_delete")
	ctx := context.Background()

	if err := mgr.ResolveConflict(ctx, resolvedID, ResolutionKeepLocal); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	// ListConflicts should return only unresolved.
	unresolved, err := mgr.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(unresolved) != 1 {
		t.Fatalf("ListConflicts: got %d, want 1", len(unresolved))
	}

	if unresolved[0].Path != "unresolved.txt" {
		t.Errorf("unresolved path = %q, want %q", unresolved[0].Path, "unresolved.txt")
	}

	// ListAllConflicts should return both.
	all, err := mgr.ListAllConflicts(ctx)
	if err != nil {
		t.Fatalf("ListAllConflicts: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("ListAllConflicts: got %d, want 2", len(all))
	}

	// Verify resolution fields are populated.
	var found bool

	for i := range all {
		if all[i].Path == "resolved-file.txt" {
			found = true

			if all[i].Resolution != ResolutionKeepLocal {
				t.Errorf("resolution = %q, want %q", all[i].Resolution, ResolutionKeepLocal)
			}

			if all[i].ResolvedBy != "user" {
				t.Errorf("resolved_by = %q, want %q", all[i].ResolvedBy, "user")
			}

			if all[i].ResolvedAt == 0 {
				t.Error("resolved_at should be non-zero")
			}
		}
	}

	if !found {
		t.Error("resolved-file.txt not found in ListAllConflicts results")
	}
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
	if !ok {
		t.Fatal("expected entry for docs/readme.md")
	}

	if entry.ItemID != "item1" {
		t.Errorf("ItemID = %q, want %q", entry.ItemID, "item1")
	}

	_, ok = b.GetByPath("nonexistent")
	if ok {
		t.Error("expected false for nonexistent path")
	}
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
	if !ok {
		t.Fatal("expected entry for key")
	}

	if got.Path != "test.txt" {
		t.Errorf("Path = %q, want %q", got.Path, "test.txt")
	}

	missingKey := driveid.NewItemKey(driveID, "nonexistent")
	_, ok = b.GetByID(missingKey)
	if ok {
		t.Error("expected false for nonexistent key")
	}
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
	if !ok {
		t.Fatal("entry not found after Put")
	}

	if got.ItemID != "item-new" {
		t.Errorf("ItemID = %q, want %q", got.ItemID, "item-new")
	}

	key := driveid.NewItemKey(driveid.New("d1"), "item-new")
	gotByID, ok := b.GetByID(key)
	if !ok {
		t.Fatal("entry not found by ID after Put")
	}

	if gotByID.Path != "new/file.txt" {
		t.Errorf("Path = %q, want %q", gotByID.Path, "new/file.txt")
	}
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

	if _, ok := b.GetByPath("delete-me.txt"); ok {
		t.Error("entry still exists after Delete")
	}

	if _, ok := b.GetByID(key); ok {
		t.Error("entry still exists in ByID after Delete")
	}

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

	if b.Len() != 2 {
		t.Errorf("Len() = %d, want 2", b.Len())
	}
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

	if len(paths) != 2 {
		t.Errorf("ForEachPath visited %d entries, want 2", len(paths))
	}

	if !paths["a.txt"] || !paths["b.txt"] {
		t.Error("ForEachPath did not visit all entries")
	}
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
	if b.Len() < 100 {
		t.Errorf("Len() = %d, want >= 100", b.Len())
	}
}
