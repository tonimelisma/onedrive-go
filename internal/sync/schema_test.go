package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.5.6
func TestNewSyncStore_CreatesCanonicalSchema(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	tables, err := listUserTables(ctx, store.DB())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"baseline",
		"local_state",
		"observation_state",
		"retry_state",
		"remote_state",
		"run_status",
		"scope_blocks",
		"sync_failures",
	}, tables)

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Zero(t, state.LastFullRemoteRefreshAt)
	assert.Zero(t, state.NextFullRemoteRefreshAt)
	assert.Zero(t, state.LastFullLocalRefreshAt)
	assert.Zero(t, state.NextFullLocalRefreshAt)
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

	require.NoError(t, applySchema(context.Background(), db))

	var rows int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM observation_state`).Scan(&rows)
	require.NoError(t, err)
	assert.Equal(t, 1, rows)
}
