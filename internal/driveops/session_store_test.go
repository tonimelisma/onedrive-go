package driveops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

// Validates: R-5.2, R-6.5.2
func TestSessionStore_ReopenPreservesUploadSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	driveID := testSessionDriveID
	localPath := "/docs/restart.bin"
	now := time.Now().UTC().Truncate(time.Second)

	store := NewSessionStore(dir, testLogger(t))
	require.NoError(t, store.Save(driveID, localPath, &SessionRecord{
		SessionURL: "https://example.com/upload/restart",
		FileHash:   "restart-hash",
		FileSize:   4096,
		CreatedAt:  now,
		MountID:    "personal:user@example.com",
		LocalRoot:  "/sync/root",
	}))

	reopened := NewSessionStore(dir, testLogger(t))
	rec, found, err := reopened.Load(driveID, localPath)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, rec)
	assert.Equal(t, "https://example.com/upload/restart", rec.SessionURL)
	assert.Equal(t, "restart-hash", rec.FileHash)
	assert.Equal(t, int64(4096), rec.FileSize)
	assert.Equal(t, "personal:user@example.com", rec.MountID)
	assert.Equal(t, "/sync/root", rec.LocalRoot)
	assert.True(t, rec.CreatedAt.Equal(now))
}

// Validates: R-5.2
func TestSessionStore_DeleteForScope_RemovesMatchingMountOrRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))
	childMountID := "parent|binding:docs"
	childRoot := filepath.Join(dir, "Parent", "Shortcuts", "Docs")
	otherRoot := filepath.Join(dir, "Other")

	require.NoError(t, store.Save(testSessionDriveID, "/child-a.bin", &SessionRecord{
		MountID:    childMountID,
		LocalRoot:  childRoot,
		SessionURL: "https://example.com/a",
		FileHash:   "a",
		FileSize:   1,
	}))
	require.NoError(t, store.Save(testSessionDriveID, "/child-b.bin", &SessionRecord{
		MountID:    "other-mount",
		LocalRoot:  filepath.Join(childRoot, "Nested"),
		SessionURL: "https://example.com/b",
		FileHash:   "b",
		FileSize:   2,
	}))
	require.NoError(t, store.Save(testSessionDriveID, "/explicit.bin", &SessionRecord{
		MountID:    "shared:explicit",
		LocalRoot:  otherRoot,
		SessionURL: "https://example.com/c",
		FileHash:   "c",
		FileSize:   3,
	}))

	deleted, err := store.DeleteForScope(childMountID, childRoot)

	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	_, found, err := store.Load(testSessionDriveID, "/child-a.bin")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = store.Load(testSessionDriveID, "/child-b.bin")
	require.NoError(t, err)
	assert.False(t, found)
	rec, found, err := store.Load(testSessionDriveID, "/explicit.bin")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, otherRoot, rec.LocalRoot)
}

// Validates: R-5.2.3
//
// Save writes the one record format Load reads: every field under its own
// key and the current version, and Load gives back what Save was given.
func TestSessionStore_SaveAndLoadRoundTripTheCurrentRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	const localPath = "/docs/round-trip.bin"
	createdAt := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	expiresAt := createdAt.Add(48 * time.Hour)
	saved := SessionRecord{
		MountID:     "mount-a",
		LocalRoot:   "/sync/root",
		SessionURL:  "https://example.com/upload/round-trip",
		FileHash:    "round-trip-hash",
		FileSize:    4096,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		ItemID:      "item-1",
		CreatedETag: `"etag,1"`,
	}
	require.NoError(t, store.Save(testSessionDriveID, localPath, &saved))

	data, err := localpath.ReadFile(store.filePath(testSessionDriveID, localPath))
	require.NoError(t, err)
	var onDisk map[string]any
	require.NoError(t, json.Unmarshal(data, &onDisk))
	assert.InDelta(t, currentSessionVersion, onDisk["version"], 0)
	assert.Equal(t, localPath, onDisk["local_path"])

	rec, found, err := store.Load(testSessionDriveID, localPath)
	require.NoError(t, err)
	require.True(t, found)
	want := saved
	want.Version = currentSessionVersion
	want.DriveID = testSessionDriveID
	want.LocalPath = localPath
	assert.Equal(t, want, *rec)
}

// Validates: R-5.2.3, R-6.6.30
//
// A session file in any other shape than the one Save writes cannot name a
// session to resume: Load removes it, warns once naming the file, the upload
// and the reason (never the pre-authenticated upload URL it may hold), and
// reports it as invalid, so the upload starts over from a fresh session.
func TestSessionStore_Load_RemovesARecordInAnyOtherFormat(t *testing.T) {
	t.Parallel()

	const (
		localPath  = "/docs/other-format.bin"
		sessionURL = "https://example.com/upload/pre-authenticated-secret"
	)
	// current is a record in today's format without its closing brace.
	current := fmt.Sprintf(`{"version":%d,"drive_id":%q,"local_path":%q,"session_url":%q,"file_hash":"h","file_size":100,"created_at":"2026-01-01T00:00:00Z"`,
		currentSessionVersion, testSessionDriveID, localPath, sessionURL)

	cases := []struct {
		name       string
		record     string
		wantReason string
	}{
		{
			name:       "not JSON",
			record:     "{not valid json",
			wantReason: "invalid character",
		},
		{
			name:       "a record cut short",
			record:     current,
			wantReason: "unexpected EOF",
		},
		{
			name:       "an empty file",
			record:     "",
			wantReason: "EOF",
		},
		{
			name: "an unversioned record keyed by remote_path",
			record: fmt.Sprintf(`{"drive_id":%q,"remote_path":%q,"session_url":%q,"file_hash":"h","file_size":100,"created_at":"2024-01-01T00:00:00Z"}`,
				testSessionDriveID, localPath, sessionURL),
			wantReason: `unknown field "remote_path"`,
		},
		{
			name: "a version 1 record keyed by remote_path",
			record: fmt.Sprintf(`{"version":1,"drive_id":%q,"remote_path":%q,"session_url":%q,"file_hash":"h","file_size":100,"created_at":"2025-06-01T00:00:00Z"}`,
				testSessionDriveID, localPath, sessionURL),
			wantReason: `unknown field "remote_path"`,
		},
		{
			name: "a version 1 record in today's shape",
			record: fmt.Sprintf(`{"version":1,"drive_id":%q,"local_path":%q,"session_url":%q,"file_hash":"h","file_size":100,"created_at":"2025-06-01T00:00:00Z"}`,
				testSessionDriveID, localPath, sessionURL),
			wantReason: "version 1",
		},
		{
			name:       "a field the record does not have",
			record:     current + `,"resumable":true}`,
			wantReason: `unknown field "resumable"`,
		},
		{
			name: "a version this binary does not know",
			record: fmt.Sprintf(`{"version":99,"drive_id":%q,"local_path":%q,"session_url":%q,"file_hash":"h","file_size":100,"created_at":"2026-01-01T00:00:00Z"}`,
				testSessionDriveID, localPath, sessionURL),
			wantReason: "version 99",
		},
		{
			name:       "a second value after the record",
			record:     current + "}{}",
			wantReason: "after the record",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			var logs bytes.Buffer
			store := NewSessionStore(dir, textLogger(&logs))
			recordPath := store.filePath(testSessionDriveID, localPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(recordPath), sessionDirPerms))
			require.NoError(t, os.WriteFile(recordPath, []byte(tc.record), sessionFilePerms))

			rec, found, err := store.Load(testSessionDriveID, localPath)
			require.ErrorIs(t, err, ErrInvalidSessionRecord)
			require.ErrorContains(t, err, tc.wantReason)
			assert.False(t, found)
			assert.Nil(t, rec)
			assert.NoFileExists(t, recordPath, "a record that cannot be resumed is removed")

			logged := unquotedLogs(&logs)
			assert.Equal(t, 1, strings.Count(logged, "level=WARN"), "one warning: %s", logged)
			assert.Contains(t, logged, filepath.Base(recordPath), "the warning names the file")
			assert.Contains(t, logged, "local_path="+localPath, "the warning names the upload that starts over")
			assert.Contains(t, logged, tc.wantReason, "the warning says why")
			assert.NotContains(t, logged, sessionURL, "the warning never carries the upload URL")
		})
	}
}

// Validates: R-5.2.3, R-6.6.30
//
// An invalid record Load cannot remove is still reported once, with the
// failure to remove it, and the upload still starts over.
func TestSessionStore_Load_ReportsAnInvalidRecordItCannotRemove(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	dir := t.TempDir()
	var logs bytes.Buffer
	store := NewSessionStore(dir, textLogger(&logs))
	const localPath = "/docs/stuck.bin"
	recordPath := store.filePath(testSessionDriveID, localPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(recordPath), sessionDirPerms))
	require.NoError(t, os.WriteFile(recordPath, []byte(`{"version":1}`), sessionFilePerms))
	makeReadOnlyForTest(t, filepath.Dir(recordPath))

	rec, found, err := store.Load(testSessionDriveID, localPath)

	require.ErrorIs(t, err, ErrInvalidSessionRecord)
	assert.False(t, found)
	assert.Nil(t, rec)
	assert.FileExists(t, recordPath)
	logged := unquotedLogs(&logs)
	assert.Equal(t, 1, strings.Count(logged, "level=WARN"), "one warning: %s", logged)
	assert.Contains(t, logged, "could not be removed")
	assert.Contains(t, logged, "version 1", "the warning says why the record is invalid")
	assert.Contains(t, logged, "permission denied", "and why it is still there")
}

// Validates: R-5.2, R-6.6.30
//
// Scope cleanup removes a session file it cannot read, with one warning
// naming the file and the reason (never the upload URL), instead of failing
// the cleanup over it on every attempt.
func TestSessionStore_DeleteForScope_RemovesAnUnreadableRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var logs bytes.Buffer
	store := NewSessionStore(dir, textLogger(&logs))
	require.NoError(t, store.Save(testSessionDriveID, "/owned.bin", &SessionRecord{
		MountID:    "mount-a",
		SessionURL: "https://example.com/owned",
	}))
	require.NoError(t, store.Save(testSessionDriveID, "/other.bin", &SessionRecord{
		MountID:    "mount-b",
		SessionURL: "https://example.com/other",
	}))
	const oldURL = "https://example.com/old-pre-authenticated"
	oldShape := store.filePath(testSessionDriveID, "/old-shape.bin")
	require.NoError(t, os.WriteFile(oldShape,
		fmt.Appendf(nil, `{"version":1,"drive_id":"drive-1","remote_path":"/old-shape.bin","session_url":%q}`, oldURL),
		sessionFilePerms))

	deleted, err := store.DeleteForScope("mount-a", "")

	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "only the scope's own record counts as deleted")
	assert.NoFileExists(t, store.filePath(testSessionDriveID, "/owned.bin"))
	assert.FileExists(t, store.filePath(testSessionDriveID, "/other.bin"))
	assert.NoFileExists(t, oldShape)
	logged := unquotedLogs(&logs)
	assert.Equal(t, 1, strings.Count(logged, "level=WARN"), "one warning: %s", logged)
	assert.Contains(t, logged, filepath.Base(oldShape), "the warning names the file")
	assert.Contains(t, logged, `unknown field "remote_path"`, "the warning says why")
	assert.NotContains(t, logged, oldURL, "the warning never carries the upload URL")
	assert.NotContains(t, logged, "starts over", "cleanup cannot know whether an upload starts over")
}

// Validates: R-5.2, R-6.6.30
//
// An invalid record scope cleanup cannot remove is returned with the
// cleanup's other failures, for its caller to report once, and the store
// does not warn about it as well.
func TestSessionStore_DeleteForScope_ReturnsAnInvalidRecordItCannotRemove(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	dir := t.TempDir()
	var logs bytes.Buffer
	store := NewSessionStore(dir, textLogger(&logs))
	invalid := store.filePath(testSessionDriveID, "/old-shape.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(invalid), sessionDirPerms))
	require.NoError(t, os.WriteFile(invalid, []byte(`{"version":1}`), sessionFilePerms))
	makeReadOnlyForTest(t, filepath.Dir(invalid))

	deleted, err := store.DeleteForScope("mount-a", "")

	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), filepath.Base(invalid))
	assert.Zero(t, deleted)
	assert.FileExists(t, invalid)
	assert.NotContains(t, logs.String(), "level=WARN")
}

// unquotedLogs is the text handler's output with the quotes it escapes inside
// quoted values (the decode reasons) put back.
func unquotedLogs(logs *bytes.Buffer) string {
	return strings.ReplaceAll(logs.String(), `\"`, `"`)
}

// Validates: R-5.2
//
// Scope cleanup reads only record files: the temporary file of a Save still
// in progress is neither a record to decode nor one to remove, so the cleanup
// neither fails over it nor pulls it from under the Save.
func TestSessionStore_DeleteForScope_LeavesASaveInProgressAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var logs bytes.Buffer
	store := NewSessionStore(dir, textLogger(&logs))
	require.NoError(t, store.Save(testSessionDriveID, "/owned.bin", &SessionRecord{
		MountID:    "mount-a",
		SessionURL: "https://example.com/owned",
	}))
	inProgress := filepath.Join(dir, sessionSubdir, strings.Replace(sessionTempPattern, "*", "123", 1))
	require.NoError(t, os.WriteFile(inProgress, []byte(`{"version":2,"drive_id"`), sessionFilePerms))

	deleted, err := store.DeleteForScope("mount-a", "")

	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.FileExists(t, inProgress)
	assert.NotContains(t, logs.String(), "level=WARN")
}

func TestSessionStore_DeleteNonexistent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))

	// Delete of non-existent file should not error.
	require.NoError(t, store.Delete(testSessionDriveID, "/nonexistent"))
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

// Validates: R-5.2
//
// A stale session that cannot be removed is reported to the caller with the
// rest of the pass's failures, not only logged inside the store.
func TestSessionStore_CleanStale_ReportsASessionItCannotRemove(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))
	require.NoError(t, store.Save(testSessionDriveID, "/stale.txt", &SessionRecord{SessionURL: "https://example.com/stale"}))
	sessionDir := filepath.Join(dir, sessionSubdir)
	stalePath := filepath.Join(sessionDir, sessionKey(testSessionDriveID, "/stale.txt"))
	staleTime := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stalePath, staleTime, staleTime))

	makeReadOnlyForTest(t, sessionDir)

	deleted, err := store.CleanStale(7 * 24 * time.Hour)
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), filepath.Base(stalePath))
	assert.Zero(t, deleted)
	assert.FileExists(t, stalePath)
}

// Validates: R-5.2
//
// A session file whose age cannot be read is reported and the rest are still
// cleaned; one that vanished since the listing is simply gone.
func TestSessionStore_RemoveStaleEntries_ReportsAnEntryItCannotStat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, testLogger(t))
	require.NoError(t, store.Save(testSessionDriveID, "/stale.txt", &SessionRecord{SessionURL: "https://example.com/stale"}))
	stalePath := filepath.Join(dir, sessionSubdir, sessionKey(testSessionDriveID, "/stale.txt"))
	staleTime := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stalePath, staleTime, staleTime))

	root, err := fsroot.Open(filepath.Join(dir, sessionSubdir))
	require.NoError(t, err)
	listed, err := root.ReadDir("")
	require.NoError(t, err)

	errUnreadable := errors.New("injected stat failure")
	entries := append([]os.DirEntry{
		infoFailingEntry{name: "unreadable.json", err: errUnreadable},
		infoFailingEntry{name: "vanished.json", err: os.ErrNotExist},
	}, listed...)

	deleted, err := store.removeStaleEntries(root, entries, time.Now().Add(-7*24*time.Hour))
	require.ErrorIs(t, err, errUnreadable)
	assert.Contains(t, err.Error(), "unreadable.json")
	assert.NotContains(t, err.Error(), "vanished.json", "a vanished session file is not a failure")
	assert.Equal(t, 1, deleted, "cleaning goes on past an entry it cannot read")
	assert.NoFileExists(t, stalePath)
}

// infoFailingEntry is a listed session file whose Info fails with err.
type infoFailingEntry struct {
	name string
	err  error
}

func (e infoFailingEntry) Name() string               { return e.name }
func (e infoFailingEntry) IsDir() bool                { return false }
func (e infoFailingEntry) Type() fs.FileMode          { return 0 }
func (e infoFailingEntry) Info() (fs.FileInfo, error) { return nil, e.err }

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
