package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.10.5
func TestReadStatusSnapshot(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES
		('last_sync_time', '2026-04-02T10:00:00Z'),
		('last_sync_duration_ms', '1500')`)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`, testDriveID)
	require.NoError(t, err)

	snapshot, err := ReadStatusSnapshot(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "2026-04-02T10:00:00Z", snapshot.SyncMetadata["last_sync_time"])
	assert.Equal(t, "1500", snapshot.SyncMetadata["last_sync_duration_ms"])
	assert.Equal(t, 1, snapshot.Issues.ConflictCount())
}

// Validates: R-2.3.6, R-2.3.12
func TestReadDurableIntentCounts_ReadOnlyDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveid.New(testDriveID),
		ActionType:    ActionRemoteDelete,
		Path:          "/delete-me.txt",
		ItemID:        "item-delete",
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('conflict-pending', ?, 'item-1', '/pending.txt', 'edit_edit', 1, 'unresolved')`,
		testDriveID,
	)
	require.NoError(t, err)

	result, err := store.RequestConflictResolution(ctx, "conflict-pending", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, result.Status)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, time.Second, 10*time.Millisecond, "WAL sidecars were not created")

	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove projection helpers stay read-only.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores permissions for tempdir removal.
		assert.NoError(t, os.Chmod(dir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	counts, err := ReadDurableIntentCounts(ctx, dbPath, newTestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, counts.PendingHeldDeleteApprovals)
	assert.Equal(t, 1, counts.PendingConflictRequests)
}

// Validates: R-2.10.47
func TestHasScopeBlockAtPath(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, IssueUnauthorized)
	require.NoError(t, err)

	hasAuthScope, err := HasScopeBlockAtPath(t.Context(), dbPath, SKAuthAccount(), newTestLogger(t))
	require.NoError(t, err)
	assert.True(t, hasAuthScope)

	hasServiceScope, err := HasScopeBlockAtPath(t.Context(), dbPath, SKService(), newTestLogger(t))
	require.NoError(t, err)
	assert.False(t, hasServiceScope)
}

func TestFinalizeInspectorRead_PreservesSuccessfulReadOnCloseError(t *testing.T) {
	t.Parallel()

	result, err := finalizeInspectorRead("state.db", newTestLogger(t), true, nil, errors.New("close failed"))
	require.NoError(t, err)
	assert.True(t, result)
}

func TestFinalizeInspectorRead_JoinsReadAndCloseErrors(t *testing.T) {
	t.Parallel()

	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")

	_, err := finalizeInspectorRead("state.db", newTestLogger(t), false, readErr, closeErr)
	require.Error(t, err)
	require.ErrorIs(t, err, readErr)
	require.ErrorIs(t, err, closeErr)
}
