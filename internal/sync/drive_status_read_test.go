package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.3.3, R-6.10.5
func TestReadDriveStatusSnapshot_ReadOnlyDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)

	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		testDriveID,
	)
	require.NoError(t, err)

	request, err := store.RequestConflictResolution(ctx, "c1", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, request.Status)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, time.Second, 10*time.Millisecond, "WAL sidecars were not created")

	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove per-drive status reads stay read-only.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores permissions for tempdir removal.
		assert.NoError(t, os.Chmod(dir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	snapshot, err := ReadDriveStatusSnapshot(ctx, dbPath, false, newTestLogger(t))
	require.NoError(t, err)
	require.Len(t, snapshot.Conflicts, 1)
	assert.Equal(t, "/conflict.txt", snapshot.Conflicts[0].Path)
	assert.Equal(t, ResolutionKeepLocal, snapshot.Conflicts[0].RequestedResolution)
	assert.Equal(t, ConflictStateQueued, snapshot.Conflicts[0].RequestState)
}
