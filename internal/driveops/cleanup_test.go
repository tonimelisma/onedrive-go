package driveops

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanTransferArtifacts_BothPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncRoot := filepath.Join(dir, "sync")
	dataDir := filepath.Join(dir, "data")

	require.NoError(t, os.MkdirAll(syncRoot, 0o700))

	// Create an owned partial file to exercise CleanStalePartials path.
	partialPath := filepath.Join(syncRoot, downloadPartialPrefix+"stale.txt"+downloadPartialSuffix)
	require.NoError(t, os.WriteFile(partialPath, []byte("x"), 0o600))

	// Create a session store with an expired session.
	store := NewSessionStore(dataDir, testLogger(t))
	err := store.Save("drive-1", "/old.txt", &SessionRecord{
		SessionURL: "https://example.com/upload/old",
		FileHash:   "h",
		FileSize:   1,
		CreatedAt:  time.Now().Add(-10 * 24 * time.Hour), // 10 days old
	})
	require.NoError(t, err)

	// Backdate the session file so CleanStale considers it stale
	// (CleanStale checks file ModTime, not the CreatedAt field).
	sessionFile := store.filePath("drive-1", "/old.txt")
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(sessionFile, oldTime, oldTime))

	// Run the unified cleanup (StaleSessionAge is inlined).
	result := CleanTransferArtifacts(mustOpenSyncTree(t, syncRoot), store, testLogger(t), TransferArtifactCleanupOptions{})
	assert.Equal(t, TransferArtifactCleanup{PartialsRemoved: 1, SessionsRemoved: 1}, result)

	// Verify the .partial file was deleted.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "partial file should have been deleted")

	// Verify the stale session was cleaned.
	rec, found, err := store.Load("drive-1", "/old.txt")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, rec, "stale session should have been cleaned")
}

// Validates: R-5.2, R-6.6.30
//
// The post-sync cleanup hands each half's failure and count to its caller and
// logs no failure itself: the engine that called it is what knows whether a
// failure is new. A refused session removal does not stop the
// partial cleanup, and a refused partial removal does not stop the sessions.
func TestCleanTransferArtifacts_ReturnsEachCleanupsFailureAndLogsNone(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	t.Run("session removal refused", func(t *testing.T) {
		t.Parallel()

		dataDir := t.TempDir()
		store := saveStaleSession(t, dataDir)
		makeReadOnlyForTest(t, filepath.Join(dataDir, sessionSubdir))
		syncRoot := t.TempDir()
		partial := filepath.Join(syncRoot, downloadPartialPrefix+"stale.txt"+downloadPartialSuffix)
		require.NoError(t, os.WriteFile(partial, []byte("x"), 0o600))

		var logs bytes.Buffer
		result := CleanTransferArtifacts(mustOpenSyncTree(t, syncRoot), store, textLogger(&logs), TransferArtifactCleanupOptions{})

		require.ErrorIs(t, result.SessionsErr, os.ErrPermission)
		assert.Zero(t, result.SessionsRemoved)
		require.NoError(t, result.PartialsErr)
		assert.Equal(t, 1, result.PartialsRemoved)
		assert.NoFileExists(t, partial)
		assert.NotContains(t, logs.String(), "level=WARN")
	})

	t.Run("partial removal refused", func(t *testing.T) {
		t.Parallel()

		store := saveStaleSession(t, t.TempDir())
		syncRoot := t.TempDir()
		locked := filepath.Join(syncRoot, "locked")
		require.NoError(t, os.Mkdir(locked, 0o700))
		partial := filepath.Join(locked, downloadPartialPrefix+"stuck.txt"+downloadPartialSuffix)
		require.NoError(t, os.WriteFile(partial, []byte("x"), 0o600))
		makeReadOnlyForTest(t, locked)

		var logs bytes.Buffer
		result := CleanTransferArtifacts(mustOpenSyncTree(t, syncRoot), store, textLogger(&logs), TransferArtifactCleanupOptions{})

		require.ErrorIs(t, result.PartialsErr, os.ErrPermission)
		assert.Zero(t, result.PartialsRemoved)
		require.NoError(t, result.SessionsErr)
		assert.Equal(t, 1, result.SessionsRemoved)
		assert.FileExists(t, partial)
		assert.NotContains(t, logs.String(), "level=WARN")
	})
}

// saveStaleSession saves one upload session under dataDir, last written
// longer ago than StaleSessionAge, and returns its store.
func saveStaleSession(t *testing.T, dataDir string) *SessionStore {
	t.Helper()

	store := NewSessionStore(dataDir, testLogger(t))
	require.NoError(t, store.Save("drive-1", "/old.txt", &SessionRecord{SessionURL: "https://example.com/upload/old"}))
	stale := time.Now().Add(-2 * StaleSessionAge)
	require.NoError(t, os.Chtimes(store.filePath("drive-1", "/old.txt"), stale, stale))

	return store
}

func textLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestCleanTransferArtifacts_NilSessionStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Should not panic with nil sessionStore.
	result := CleanTransferArtifacts(mustOpenSyncTree(t, dir), nil, testLogger(t), TransferArtifactCleanupOptions{})
	assert.Equal(t, TransferArtifactCleanup{}, result)
}
