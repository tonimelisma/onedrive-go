package driveops

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.Default()
}

func TestSessionStore_SaveLoadDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-1"
	localPath := "/docs/report.docx"

	// Load from empty store returns nil.
	rec, err := store.Load(driveID, localPath)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}

	if rec != nil {
		t.Fatal("expected nil record from empty store")
	}

	// Save a record.
	now := time.Now().UTC().Truncate(time.Second)
	err = store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/upload/abc",
		FileHash:   "hash123",
		FileSize:   1024,
		CreatedAt:  now,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load it back.
	rec, err = store.Load(driveID, localPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if rec == nil {
		t.Fatal("expected non-nil record")
	}

	if rec.DriveID != driveID {
		t.Errorf("DriveID = %q, want %q", rec.DriveID, driveID)
	}

	if rec.LocalPath != localPath {
		t.Errorf("LocalPath = %q, want %q", rec.LocalPath, localPath)
	}

	if rec.SessionURL != "https://example.com/upload/abc" {
		t.Errorf("SessionURL = %q, want %q", rec.SessionURL, "https://example.com/upload/abc")
	}

	if rec.FileHash != "hash123" {
		t.Errorf("FileHash = %q, want %q", rec.FileHash, "hash123")
	}

	if rec.FileSize != 1024 {
		t.Errorf("FileSize = %d, want %d", rec.FileSize, 1024)
	}

	if !rec.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", rec.CreatedAt, now)
	}

	// Delete it.
	delErr := store.Delete(driveID, localPath)
	if delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}

	// Load after delete returns nil.
	rec, err = store.Load(driveID, localPath)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}

	if rec != nil {
		t.Fatal("expected nil record after delete")
	}
}

func TestSessionStore_DeleteNonexistent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Delete of non-existent file should not error.
	if err := store.Delete("drive-1", "/nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

func TestSessionStore_CorruptFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-1"
	localPath := "/corrupt.txt"

	// Write a corrupt JSON file at the expected path.
	sessionDir := filepath.Join(dir, sessionSubdir)
	if err := os.MkdirAll(sessionDir, sessionDirPerms); err != nil {
		t.Fatal(err)
	}

	key := sessionKey(driveID, localPath)
	corruptPath := filepath.Join(sessionDir, key)

	if err := os.WriteFile(corruptPath, []byte("{not valid json"), sessionFilePerms); err != nil {
		t.Fatal(err)
	}

	// Load should return ErrCorruptSession (corrupt file deleted).
	rec, err := store.Load(driveID, localPath)
	if !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("Load corrupt: expected ErrCorruptSession, got %v", err)
	}

	if rec != nil {
		t.Fatal("expected nil record from corrupt file")
	}

	// Corrupt file should have been cleaned up.
	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Error("corrupt file was not deleted")
	}
}

func TestSessionStore_Overwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-1"
	localPath := "/overwrite.txt"

	// Save first record.
	err := store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/old",
		FileHash:   "old-hash",
		FileSize:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Overwrite with new record.
	err = store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/new",
		FileHash:   "new-hash",
		FileSize:   200,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Load should return the new record.
	rec, err := store.Load(driveID, localPath)
	if err != nil {
		t.Fatal(err)
	}

	if rec.SessionURL != "https://example.com/new" {
		t.Errorf("SessionURL = %q, want new URL", rec.SessionURL)
	}

	if rec.FileHash != "new-hash" {
		t.Errorf("FileHash = %q, want new-hash", rec.FileHash)
	}
}

func TestSessionStore_DifferentKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Two different local paths should produce different session files.
	err := store.Save("drive-1", "/path-a", &SessionRecord{
		SessionURL: "https://example.com/a",
		FileHash:   "hash-a",
		FileSize:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.Save("drive-1", "/path-b", &SessionRecord{
		SessionURL: "https://example.com/b",
		FileHash:   "hash-b",
		FileSize:   200,
	})
	if err != nil {
		t.Fatal(err)
	}

	recA, err := store.Load("drive-1", "/path-a")
	if err != nil {
		t.Fatal(err)
	}

	recB, err := store.Load("drive-1", "/path-b")
	if err != nil {
		t.Fatal(err)
	}

	if recA.FileHash != "hash-a" || recB.FileHash != "hash-b" {
		t.Error("records mixed up")
	}
}

func TestSessionStore_CleanStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Pre-set lastClean so Save's lazy cleanup goroutine is throttled and
	// won't interfere with our explicit CleanStale call below.
	store.cleanMu.Lock()
	store.lastClean = time.Now()
	store.cleanMu.Unlock()

	// Save a session record.
	err := store.Save("drive-1", "/stale.txt", &SessionRecord{
		SessionURL: "https://example.com/stale",
		FileHash:   "hash-stale",
		FileSize:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Back-date the file to make it stale.
	sessionDir := filepath.Join(dir, sessionSubdir)
	key := sessionKey("drive-1", "/stale.txt")
	filePath := filepath.Join(sessionDir, key)
	staleTime := time.Now().Add(-8 * 24 * time.Hour)

	chtErr := os.Chtimes(filePath, staleTime, staleTime)
	if chtErr != nil {
		t.Fatal(chtErr)
	}

	// Save a fresh session that should NOT be cleaned.
	err = store.Save("drive-1", "/fresh.txt", &SessionRecord{
		SessionURL: "https://example.com/fresh",
		FileHash:   "hash-fresh",
		FileSize:   200,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Clean with 7-day TTL.
	deleted, err := store.CleanStale(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanStale: %v", err)
	}

	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	// Stale session should be gone.
	rec, err := store.Load("drive-1", "/stale.txt")
	if err != nil {
		t.Fatal(err)
	}

	if rec != nil {
		t.Error("stale session still exists after cleanup")
	}

	// Fresh session should remain.
	rec, err = store.Load("drive-1", "/fresh.txt")
	if err != nil {
		t.Fatal(err)
	}

	if rec == nil {
		t.Error("fresh session was incorrectly deleted")
	}
}

func TestSessionStore_CleanStale_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// CleanStale on non-existent directory should not error.
	deleted, err := store.CleanStale(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanStale empty: %v", err)
	}

	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

func TestSessionStore_FilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	err := store.Save("drive-1", "/perms.txt", &SessionRecord{
		SessionURL: "https://example.com/perms",
		FileHash:   "hash",
		FileSize:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check file permissions.
	key := sessionKey("drive-1", "/perms.txt")
	filePath := filepath.Join(dir, sessionSubdir, key)

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}

	perm := info.Mode().Perm()
	if perm != sessionFilePerms {
		t.Errorf("file perms = %o, want %o", perm, sessionFilePerms)
	}
}

func TestSessionStore_SaveSetsCreatedAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	before := time.Now().UTC()

	err := store.Save("drive-1", "/auto-time.txt", &SessionRecord{
		SessionURL: "https://example.com/auto",
		FileHash:   "hash",
		FileSize:   100,
		// CreatedAt intentionally zero — Save should populate it.
	})
	if err != nil {
		t.Fatal(err)
	}

	after := time.Now().UTC()

	rec, err := store.Load("drive-1", "/auto-time.txt")
	if err != nil {
		t.Fatal(err)
	}

	if rec.CreatedAt.Before(before) || rec.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", rec.CreatedAt, before, after)
	}
}

func TestSessionKey_Deterministic(t *testing.T) {
	t.Parallel()

	key1 := sessionKey("drive-1", "/path/file.txt")
	key2 := sessionKey("drive-1", "/path/file.txt")

	if key1 != key2 {
		t.Errorf("non-deterministic keys: %q vs %q", key1, key2)
	}

	// Different inputs produce different keys.
	key3 := sessionKey("drive-2", "/path/file.txt")
	if key1 == key3 {
		t.Error("different drive IDs produced same key")
	}

	key4 := sessionKey("drive-1", "/other/file.txt")
	if key1 == key4 {
		t.Error("different paths produced same key")
	}
}

func TestSessionKey_NoDelimiterCollision(t *testing.T) {
	t.Parallel()

	// "a:" + "b" must differ from "a" + ":b" — the length-prefixed format prevents this.
	key1 := sessionKey("a:", "b")
	key2 := sessionKey("a", ":b")

	if key1 == key2 {
		t.Error("delimiter collision: sessionKey(\"a:\", \"b\") == sessionKey(\"a\", \":b\")")
	}
}

func TestSessionStore_AtomicSave_NoTmpLeftover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Throttle cleanIfDue to prevent goroutine interference.
	store.cleanMu.Lock()
	store.lastClean = time.Now()
	store.cleanMu.Unlock()

	err := store.Save("drive-1", "/atomic.txt", &SessionRecord{
		SessionURL: "https://example.com/atomic",
		FileHash:   "hash",
		FileSize:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify no .tmp files remain after successful save.
	sessionDir := filepath.Join(dir, sessionSubdir)

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover .tmp file: %s", e.Name())
		}
	}
}

func TestSessionStore_ConcurrentSaveLoadDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Throttle cleanIfDue to prevent goroutine interference.
	store.cleanMu.Lock()
	store.lastClean = time.Now()
	store.cleanMu.Unlock()

	const workers = 8
	const iterations = 50

	var wg sync.WaitGroup

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			drive := fmt.Sprintf("drive-%d", w)
			path := fmt.Sprintf("/file-%d.txt", w)

			for range iterations {
				// Save.
				err := store.Save(drive, path, &SessionRecord{
					SessionURL: "https://example.com/session",
					FileHash:   "hash",
					FileSize:   100,
				})
				if err != nil {
					t.Errorf("Save: %v", err)
					return
				}

				// Load.
				rec, err := store.Load(drive, path)
				if err != nil {
					t.Errorf("Load: %v", err)
					return
				}

				if rec == nil {
					// Concurrent delete may have removed it.
					continue
				}

				if rec.FileHash != "hash" {
					t.Errorf("unexpected hash: %q", rec.FileHash)
				}

				// Delete.
				if err := store.Delete(drive, path); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
}
