package syncstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.5
func TestInspector_ReadStatusSnapshot(t *testing.T) {
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
		('last_sync_duration_ms', '1500'),
		('last_sync_error', 'network timeout')`)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO baseline
		(path, drive_id, item_id, parent_id, item_type, synced_at)
		VALUES ('/a.txt', ?, 'item-1', 'root', 'file', 1)`, testDriveID)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-2', '/conflict.txt', 'edit_edit', 1, 'unresolved')`, testDriveID)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
		VALUES
		(?, 'item-3', '/pending.txt', 'root', 'file', 'pending_download', 1),
		(?, 'item-4', '/synced.txt', 'root', 'file', 'synced', 1)`,
		testDriveID, testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/retry.txt', ?, 'upload', 'upload', 'item', 'transient', 4, 1, 1),
		('/actionable.txt', ?, 'upload', 'upload', 'item', 'actionable', 1, 1, 1)`,
		testDriveID, testDriveID,
	)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, "2026-04-02T10:00:00Z", snapshot.SyncMetadata["last_sync_time"])
	assert.Equal(t, "1500", snapshot.SyncMetadata["last_sync_duration_ms"])
	assert.Equal(t, "network timeout", snapshot.SyncMetadata["last_sync_error"])
	assert.Equal(t, 1, snapshot.BaselineEntryCount)
	assert.Equal(t, 1, snapshot.UnresolvedConflicts)
	assert.Equal(t, 1, snapshot.ActionableFailures)
	assert.Equal(t, 1, snapshot.PendingSyncItems)
	assert.Equal(t, 1, snapshot.RetryingFailures)
}
