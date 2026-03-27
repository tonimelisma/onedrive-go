package driveops

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSessionDriveID = "drive-1"

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.DiscardHandler)
}

// Validates: R-5.2
func TestSessionStore_SaveLoadDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := testSessionDriveID
	localPath := "/docs/report.docx"

	// Load from empty store returns nil.
	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.False(t, found)
	require.Nil(t, rec)

	// Save a record.
	now := time.Now().UTC().Truncate(time.Second)
	err = store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/upload/abc",
		FileHash:   "hash123",
		FileSize:   1024,
		CreatedAt:  now,
	})
	require.NoError(t, err)

	// Load it back.
	rec, found, err = store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, rec)

	assert.Equal(t, driveID, rec.DriveID)
	assert.Equal(t, localPath, rec.LocalPath)
	assert.Equal(t, "https://example.com/upload/abc", rec.SessionURL)
	assert.Equal(t, "hash123", rec.FileHash)
	assert.Equal(t, int64(1024), rec.FileSize)
	assert.True(t, rec.CreatedAt.Equal(now), "CreatedAt = %v, want %v", rec.CreatedAt, now)

	// Delete it.
	require.NoError(t, store.Delete(driveID, localPath))

	// Load after delete returns nil.
	rec, found, err = store.Load(driveID, localPath)
	require.NoError(t, err)
	require.False(t, found)
	require.Nil(t, rec)
}

func TestSessionStore_VersionField(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-ver"
	localPath := "/docs/versioned.txt"

	// Save sets Version to currentSessionVersion.
	err := store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/upload/ver",
		FileHash:   "vhash",
		FileSize:   512,
	})
	require.NoError(t, err)

	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, currentSessionVersion, rec.Version)
}

func TestSessionStore_OldFormatVersionZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Write a JSON file without the "version" key (simulating old format).
	driveID := "drive-old"
	localPath := "/docs/old.txt"

	sessDir := filepath.Join(dir, "upload-sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	oldJSON := `{"drive_id":"drive-old","remote_path":"/docs/old.txt","session_url":"https://old","file_hash":"h","file_size":100,"created_at":"2024-01-01T00:00:00Z"}`
	fpath := store.filePath(driveID, localPath)
	require.NoError(t, os.WriteFile(fpath, []byte(oldJSON), 0o600))

	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)

	// Old format has no "version" key — JSON unmarshal leaves int at zero.
	assert.Equal(t, 0, rec.Version)

	// B-300: LocalPath should be populated from old "remote_path" key.
	assert.Equal(t, "/docs/old.txt", rec.LocalPath)
}

// TestSessionStore_V1ToV2Migration verifies that v1 JSON files with the
// "remote_path" key are correctly loaded and the LocalPath field is populated (B-300).
func TestSessionStore_V1ToV2Migration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-v1"
	localPath := "/docs/migrated.txt"

	sessDir := filepath.Join(dir, "upload-sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	// v1 format: has "version":1 and "remote_path" key.
	v1JSON := `{"version":1,"drive_id":"drive-v1","remote_path":"/docs/migrated.txt","session_url":"https://v1","file_hash":"vh","file_size":256,"created_at":"2025-06-01T00:00:00Z"}`
	fpath := store.filePath(driveID, localPath)
	require.NoError(t, os.WriteFile(fpath, []byte(v1JSON), 0o600))

	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, 1, rec.Version)
	assert.Equal(t, "/docs/migrated.txt", rec.LocalPath)
	assert.Equal(t, "https://v1", rec.SessionURL)
}

// TestSessionStore_V2RoundTrip verifies that v2 records with "local_path" key
// round-trip correctly through Save and Load (B-300).
func TestSessionStore_V2RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-v2"
	localPath := "/docs/v2file.txt"

	err := store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://v2session",
		FileHash:   "v2hash",
		FileSize:   512,
	})
	require.NoError(t, err)

	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, currentSessionVersion, rec.Version)
	assert.Equal(t, localPath, rec.LocalPath)
	assert.Equal(t, "https://v2session", rec.SessionURL)
}

// TestSessionStore_SaveWritesV2JSON verifies that Save writes the v2 JSON tag
// "local_path" (not the old "remote_path") (B-300).
func TestSessionStore_SaveWritesV2JSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-v2write"
	localPath := "/docs/written.txt"

	err := store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://write-test",
		FileHash:   "wh",
		FileSize:   64,
	})
	require.NoError(t, err)

	// Read raw JSON to verify key names.
	fpath := store.filePath(driveID, localPath)
	data, err := os.ReadFile(fpath) //nolint:gosec // Test session file lives under t.TempDir and is controlled by the test.
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"local_path"`, "saved JSON should contain \"local_path\" key, got: %s", raw)
	assert.NotContains(t, raw, `"remote_path"`, "saved JSON should NOT contain \"remote_path\" key, got: %s", raw)
}

func TestSessionStore_DeleteNonexistent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Delete of non-existent file should not error.
	require.NoError(t, store.Delete(testSessionDriveID, "/nonexistent"))
}

func TestSessionStore_CorruptFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := testSessionDriveID
	localPath := "/corrupt.txt"

	// Write a corrupt JSON file at the expected path.
	sessionDir := filepath.Join(dir, sessionSubdir)
	require.NoError(t, os.MkdirAll(sessionDir, sessionDirPerms))

	key := sessionKey(driveID, localPath)
	corruptPath := filepath.Join(sessionDir, key)
	require.NoError(t, os.WriteFile(corruptPath, []byte("{not valid json"), sessionFilePerms))

	// Load should return ErrCorruptSession (corrupt file deleted).
	rec, found, err := store.Load(driveID, localPath)
	require.ErrorIs(t, err, ErrCorruptSession)
	require.False(t, found)
	require.Nil(t, rec)

	// Corrupt file should have been cleaned up.
	_, statErr := os.Stat(corruptPath)
	assert.True(t, os.IsNotExist(statErr), "corrupt file was not deleted")
}

func TestSessionStore_Overwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := testSessionDriveID
	localPath := "/overwrite.txt"

	// Save first record.
	require.NoError(t, store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/old",
		FileHash:   "old-hash",
		FileSize:   100,
	}))

	// Overwrite with new record.
	require.NoError(t, store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/new",
		FileHash:   "new-hash",
		FileSize:   200,
	}))

	// Load should return the new record.
	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, "https://example.com/new", rec.SessionURL)
	assert.Equal(t, "new-hash", rec.FileHash)
}

func TestSessionStore_DifferentKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Two different local paths should produce different session files.
	require.NoError(t, store.Save(testSessionDriveID, "/path-a", &SessionRecord{
		SessionURL: "https://example.com/a",
		FileHash:   "hash-a",
		FileSize:   100,
	}))

	require.NoError(t, store.Save(testSessionDriveID, "/path-b", &SessionRecord{
		SessionURL: "https://example.com/b",
		FileHash:   "hash-b",
		FileSize:   200,
	}))

	recA, foundA, err := store.Load(testSessionDriveID, "/path-a")
	require.NoError(t, err)
	require.True(t, foundA)

	recB, foundB, err := store.Load(testSessionDriveID, "/path-b")
	require.NoError(t, err)
	require.True(t, foundB)

	assert.Equal(t, "hash-a", recA.FileHash)
	assert.Equal(t, "hash-b", recB.FileHash)
}

func TestSessionStore_Load_StaleSessionReturnsNil(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	driveID := "drive-stale"
	localPath := "/docs/old.docx"

	// Save a valid session.
	require.NoError(t, store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/upload/stale",
		FileHash:   "hash-stale",
		FileSize:   512,
	}))

	// Age the file beyond StaleSessionAge.
	sessionPath := store.filePath(driveID, localPath)
	oldTime := time.Now().Add(-StaleSessionAge - time.Hour)
	require.NoError(t, os.Chtimes(sessionPath, oldTime, oldTime))

	// Load should return nil (session expired) and delete the file.
	rec, found, err := store.Load(driveID, localPath)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, rec, "stale session should return nil")

	// File should be deleted.
	_, statErr := os.Stat(sessionPath)
	assert.True(t, os.IsNotExist(statErr), "stale session file should be deleted")
}

// Validates: R-5.2
func TestSessionStore_CleanStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Save a session record.
	require.NoError(t, store.Save(testSessionDriveID, "/stale.txt", &SessionRecord{
		SessionURL: "https://example.com/stale",
		FileHash:   "hash-stale",
		FileSize:   100,
	}))

	// Back-date the file to make it stale.
	sessionDir := filepath.Join(dir, sessionSubdir)
	key := sessionKey(testSessionDriveID, "/stale.txt")
	filePath := filepath.Join(sessionDir, key)
	staleTime := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(filePath, staleTime, staleTime))

	// Save a fresh session that should NOT be cleaned.
	require.NoError(t, store.Save(testSessionDriveID, "/fresh.txt", &SessionRecord{
		SessionURL: "https://example.com/fresh",
		FileHash:   "hash-fresh",
		FileSize:   200,
	}))

	// Clean with 7-day TTL.
	deleted, err := store.CleanStale(7 * 24 * time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	// Stale session should be gone.
	rec, found, err := store.Load(testSessionDriveID, "/stale.txt")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, rec, "stale session still exists after cleanup")

	// Fresh session should remain.
	rec, found, err = store.Load(testSessionDriveID, "/fresh.txt")
	require.NoError(t, err)
	assert.True(t, found)
	assert.NotNil(t, rec, "fresh session was incorrectly deleted")
}

func TestSessionStore_CleanStale_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// CleanStale on non-existent directory should not error.
	deleted, err := store.CleanStale(7 * 24 * time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted)
}

func TestSessionStore_FilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	require.NoError(t, store.Save(testSessionDriveID, "/perms.txt", &SessionRecord{
		SessionURL: "https://example.com/perms",
		FileHash:   "hash",
		FileSize:   100,
	}))

	// Check file permissions.
	key := sessionKey(testSessionDriveID, "/perms.txt")
	filePath := filepath.Join(dir, sessionSubdir, key)

	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(sessionFilePerms), info.Mode().Perm())
}

func TestSessionStore_SaveSetsCreatedAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	before := time.Now().UTC()

	require.NoError(t, store.Save(testSessionDriveID, "/auto-time.txt", &SessionRecord{
		SessionURL: "https://example.com/auto",
		FileHash:   "hash",
		FileSize:   100,
		// CreatedAt intentionally zero — Save should populate it.
	}))

	after := time.Now().UTC()

	rec, found, err := store.Load(testSessionDriveID, "/auto-time.txt")
	require.NoError(t, err)
	require.True(t, found)

	assert.False(t, rec.CreatedAt.Before(before), "CreatedAt %v is before %v", rec.CreatedAt, before)
	assert.False(t, rec.CreatedAt.After(after), "CreatedAt %v is after %v", rec.CreatedAt, after)
}

func TestSessionKey_Deterministic(t *testing.T) {
	t.Parallel()

	key1 := sessionKey(testSessionDriveID, "/path/file.txt")
	key2 := sessionKey(testSessionDriveID, "/path/file.txt")

	assert.Equal(t, key1, key2)

	// Different inputs produce different keys.
	key3 := sessionKey("drive-2", "/path/file.txt")
	assert.NotEqual(t, key1, key3, "different drive IDs produced same key")

	key4 := sessionKey(testSessionDriveID, "/other/file.txt")
	assert.NotEqual(t, key1, key4, "different paths produced same key")
}

func TestSessionKey_NoDelimiterCollision(t *testing.T) {
	t.Parallel()

	// "a:" + "b" must differ from "a" + ":b" — the length-prefixed format prevents this.
	key1 := sessionKey("a:", "b")
	key2 := sessionKey("a", ":b")

	assert.NotEqual(t, key1, key2, "delimiter collision: sessionKey(\"a:\", \"b\") == sessionKey(\"a\", \":b\")")
}

func TestSessionStore_AtomicSave_NoTmpLeftover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	require.NoError(t, store.Save(testSessionDriveID, "/atomic.txt", &SessionRecord{
		SessionURL: "https://example.com/atomic",
		FileHash:   "hash",
		FileSize:   100,
	}))

	// Verify no .tmp files remain after successful save.
	sessionDir := filepath.Join(dir, sessionSubdir)

	entries, err := os.ReadDir(sessionDir)
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotEqual(t, ".tmp", filepath.Ext(e.Name()), "leftover .tmp file: %s", e.Name())
	}
}

func TestSessionStore_ConcurrentSaveLoadDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

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
				if !assert.NoError(t, err, "Save") {
					return
				}

				// Load.
				rec, found, err := store.Load(drive, path)
				if !assert.NoError(t, err, "Load") {
					return
				}

				if !found {
					// Concurrent delete may have removed it.
					continue
				}

				assert.Equal(t, "hash", rec.FileHash)

				// Delete.
				if !assert.NoError(t, store.Delete(drive, path), "Delete") {
					return
				}
			}
		}()
	}

	wg.Wait()
}
