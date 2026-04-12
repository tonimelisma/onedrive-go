package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.47
func TestRepairStateDB_RepairsReadableStoreInPlace(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "repair.db")
	logger := newTestLogger(t)
	store, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	ctx := t.Context()
	seedRepairIntegrityProblems(t, store, ctx, driveid.New(testDriveID))
	require.NoError(t, store.Close(context.Background()))

	result, err := RepairStateDB(ctx, dbPath, logger)
	require.NoError(t, err)
	assert.Equal(t, StateDBRepairRepair, result.Action)
	assert.Positive(t, result.RepairsApplied)

	reopened, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	report, err := reopened.AuditIntegrity(ctx)
	require.NoError(t, err)
	assert.False(t, report.HasFindings())
}

// Validates: R-2.3.6, R-2.3.12
func TestRepairStateDB_RebuildPreservesDurableIntent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "rebuild.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE conflicts (
			id TEXT PRIMARY KEY,
			drive_id TEXT NOT NULL,
			item_id TEXT,
			path TEXT NOT NULL,
			conflict_type TEXT NOT NULL,
			detected_at INTEGER NOT NULL,
			local_hash TEXT,
			remote_hash TEXT,
			local_mtime INTEGER,
			remote_mtime INTEGER,
			resolution TEXT NOT NULL,
			resolved_at INTEGER,
			resolved_by TEXT
		);
		CREATE TABLE conflict_requests (
			conflict_id TEXT PRIMARY KEY,
			requested_resolution TEXT NOT NULL,
			state TEXT NOT NULL,
			requested_at INTEGER,
			applying_at INTEGER,
			last_error TEXT
		);
		CREATE TABLE held_deletes (
			drive_id TEXT NOT NULL,
			action_type TEXT NOT NULL,
			path TEXT NOT NULL,
			item_id TEXT NOT NULL,
			state TEXT NOT NULL,
			held_at INTEGER NOT NULL,
			approved_at INTEGER,
			last_planned_at INTEGER NOT NULL,
			last_error TEXT,
			PRIMARY KEY (drive_id, action_type, path, item_id)
		);
		INSERT INTO conflicts (
			id, drive_id, item_id, path, conflict_type, detected_at, resolution
		) VALUES (
			'conflict-a', ?, 'item-a', '/conflict-a.txt', 'edit_edit', 1, 'unresolved'
		);
		INSERT INTO conflict_requests (
			conflict_id, requested_resolution, state, requested_at, applying_at, last_error
		) VALUES (
			'conflict-a', 'keep_local', 'queued', 2, NULL, 'temporary upload failure'
		);
		INSERT INTO held_deletes (
			drive_id, action_type, path, item_id, state, held_at, approved_at, last_planned_at, last_error
		) VALUES (
			?, 'remote_delete', '/delete-me.txt', 'item-delete', 'approved', 3, 4, 5, ''
		);`,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	result, err := RepairStateDB(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, StateDBRepairRebuild, result.Action)
	assert.Equal(t, 1, result.PreservedConflicts)
	assert.Equal(t, 1, result.PreservedRequests)
	assert.Equal(t, 1, result.PreservedHeldDeletes)

	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	conflict, err := store.GetConflict(t.Context(), "conflict-a")
	require.NoError(t, err)
	assert.Equal(t, ResolutionUnresolved, conflict.Resolution)
	assert.Equal(t, "item-a", conflict.ItemID)

	request, err := store.GetConflictRequest(t.Context(), "conflict-a")
	require.NoError(t, err)
	assert.Equal(t, ConflictStateQueued, request.State)
	assert.Equal(t, ResolutionKeepLocal, request.RequestedResolution)
	assert.Equal(t, "temporary upload failure", request.LastError)

	approved, err := store.ListHeldDeletesByState(t.Context(), HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "item-delete", approved[0].ItemID)

	var baselineCount int
	err = store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM baseline`).Scan(&baselineCount)
	require.NoError(t, err)
	assert.Zero(t, baselineCount)
}

// Validates: R-2.10.47
func TestRepairStateDB_ResetsWhenExistingDatabaseCannotBeSalvaged(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "broken.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600))

	result, err := RepairStateDB(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, StateDBRepairReset, result.Action)

	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	report, err := store.AuditIntegrity(t.Context())
	require.NoError(t, err)
	assert.False(t, report.HasFindings())

	conflicts, err := store.ListConflicts(t.Context())
	require.NoError(t, err)
	assert.Empty(t, conflicts)

	heldDeletes, err := store.ListHeldDeletesByState(t.Context(), HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, heldDeletes)
}
