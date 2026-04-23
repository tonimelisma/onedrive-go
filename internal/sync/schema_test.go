package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.5.6
func TestNewSyncStore_CreatesCanonicalSchema(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	tables, err := listUserTables(ctx, store.rawDB())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"baseline",
		"block_scopes",
		"local_state",
		"observation_issues",
		"observation_state",
		"remote_state",
		"retry_work",
		"store_metadata",
	}, tables)

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Zero(t, state.NextFullRemoteRefreshAt)
}

// Validates: R-2.5.6
func TestNewSyncStore_RejectsNonCanonicalSchema(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE baseline (
			item_id TEXT NOT NULL,
			path TEXT NOT NULL PRIMARY KEY
		);
		CREATE TABLE legacy_shadow (
			path TEXT NOT NULL PRIMARY KEY,
			value TEXT NOT NULL
		)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.Error(t, err)
	require.Nil(t, store)
	require.ErrorIs(t, err, ErrIncompatibleSchema)
	assert.Contains(t, err.Error(), "current schema")
}

// Validates: R-2.5.6
func TestApplySchema_SeedsObservationStateRowForCurrentSchema(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	_, err = db.ExecContext(t.Context(), canonicalSchemaSQL)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, currentSyncStoreGeneration)
	require.NoError(t, err)

	require.NoError(t, applySchema(context.Background(), db))

	var rows int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM observation_state`).Scan(&rows)
	require.NoError(t, err)
	assert.Equal(t, 1, rows)
}

// Validates: R-2.5.6
func TestApplySchema_RejectsGeneration6RemoteStateDriveOwnership(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	legacySchema := strings.Replace(canonicalSchemaSQL, "    drive_id      TEXT    NOT NULL DEFAULT '',\n", "", 1)
	_, err = db.ExecContext(t.Context(), legacySchema)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, 6)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureObservationStateRow)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		UPDATE observation_state
		SET configured_drive_id = 'drive-configured'`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO remote_state (item_id, path, item_type, hash)
		VALUES ('legacy-item', 'legacy.txt', 'file', 'legacy-hash')`)
	require.NoError(t, err)

	err = applySchema(t.Context(), db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incompatible sync store schema")
}
