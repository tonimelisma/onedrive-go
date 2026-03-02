package driveops

import (
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

	// Create a .partial file to exercise CleanStalePartials path.
	partialPath := filepath.Join(syncRoot, "stale.partial")
	require.NoError(t, os.WriteFile(partialPath, []byte("x"), 0o644))

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
	CleanTransferArtifacts(syncRoot, store, testLogger(t))

	// Verify the .partial file was deleted.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "partial file should have been deleted")

	// Verify the stale session was cleaned.
	rec, err := store.Load("drive-1", "/old.txt")
	require.NoError(t, err)
	assert.Nil(t, rec, "stale session should have been cleaned")
}

func TestCleanTransferArtifacts_NilSessionStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Should not panic with nil sessionStore.
	CleanTransferArtifacts(dir, nil, testLogger(t))
}
