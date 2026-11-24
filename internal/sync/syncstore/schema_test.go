package syncstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Validates: R-2.5.6, R-2.3.2
func TestNewSyncStore_CreatesCanonicalSchema(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	tables, err := listUserTables(ctx, store.db)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"baseline",
		"block_scopes",
		"delete_ledger",
		"held_deletes",
		"kept_local_folders",
		"local_hash_cache",
		"local_move_intents",
		"local_state",
		"mount_boundaries",
		"observation_issues",
		"observation_state",
		"pending_creates",
		"remote_state",
		"retry_work",
		"shortcut_roots",
		"store_metadata",
	}, tables)

	columns, err := listTableColumns(ctx, store.db, "observation_state")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"content_drive_id",
		"cursor",
		"local_truth_complete",
		"local_truth_recovery_reason",
		"next_full_remote_refresh_at",
		"sync_root_bound",
		"sync_root_device",
		"sync_root_inode",
		"sync_root_fsid",
		"sync_root_birth_time",
		"sync_root_token",
		"sync_root_rebind",
		"undecided_refresh_streak",
	}, columns)

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Zero(t, state.NextFullRemoteRefreshAt)
}

// requireNewSyncStoreRefusesSchema writes ddl into a fresh database file and
// asserts that opening it as a sync store refuses it as an incompatible
// schema rather than adopting or rewriting it.
func requireNewSyncStoreRefusesSchema(t *testing.T, ddl string) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), ddl)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store, err := NewSyncStore(t.Context(), dbPath, syncfixture.NewTestLogger(t))
	require.Error(t, err)
	require.Nil(t, store)
	require.ErrorIs(t, err, errIncompatibleSchema)
	assert.Contains(t, err.Error(), "current schema")
}

// Validates: R-2.5.6
func TestNewSyncStore_RejectsNonCanonicalSchema(t *testing.T) {
	t.Parallel()

	requireNewSyncStoreRefusesSchema(t, `
		CREATE TABLE baseline (
			item_id TEXT NOT NULL,
			path TEXT NOT NULL PRIMARY KEY
		);
		CREATE TABLE legacy_shadow (
			path TEXT NOT NULL PRIMARY KEY,
			value TEXT NOT NULL
		)`)
}

// Validates: R-2.5.6
func TestApplySchema_SeedsObservationStateRowForCurrentSchema(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	_, err = db.ExecContext(t.Context(), CanonicalSchemaSQLForTest())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, currentSyncStoreGeneration)
	require.NoError(t, err)

	require.NoError(t, applySchema(context.Background(), db))

	var rows int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM observation_state`).Scan(&rows)
	require.NoError(t, err)
	assert.Equal(t, 1, rows)
}

// Validates: R-2.5.6, R-2.5.9
func TestApplySchema_RejectsGeneration6RemoteStateDriveOwnership(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", SqliteFileURI(dbPath, nil))
	require.NoError(t, err)

	legacySchema := strings.Replace(CanonicalSchemaSQLForTest(), "    drive_id      TEXT    NOT NULL DEFAULT '',\n", "", 1)
	_, err = db.ExecContext(t.Context(), legacySchema)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, 6)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureObservationStateRow)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		UPDATE observation_state
		SET content_drive_id = 'drive-mounted'`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO remote_state (item_id, path, item_type, hash)
		VALUES ('legacy-item', 'legacy.txt', 'file', 'legacy-hash')`)
	require.NoError(t, err)

	err = applySchema(t.Context(), db)
	require.ErrorIs(t, err, errIncompatibleSchema)
	var generationErr *storeGenerationError
	require.ErrorAs(t, err, &generationErr, "the stored generation decides before the columns are compared")
	assert.True(t, generationErr.IsOlder())
}

// A store stamped with a generation this version does not know yet was
// written by a newer onedrive-go, whatever its columns.
//
// Validates: R-2.5.9
func TestApplySchema_NewerGenerationIsNamedNewer(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", SqliteFileURI(filepath.Join(t.TempDir(), "state.db"), nil))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	_, err = db.ExecContext(t.Context(), CanonicalSchemaSQLForTest()+`; CREATE TABLE future_table (path TEXT)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, currentSyncStoreGeneration+1)
	require.NoError(t, err)

	err = applySchema(t.Context(), db)
	require.ErrorIs(t, err, errIncompatibleSchema)
	var generationErr *storeGenerationError
	require.ErrorAs(t, err, &generationErr)
	assert.False(t, generationErr.IsOlder())
	assert.Equal(t, currentSyncStoreGeneration+1, generationErr.stored)
}

// A store at the current generation whose shape does not match was written by
// a build that changed the schema without a new generation: neither older nor
// newer, so it is refused for an explicit reset.
//
// Validates: R-2.5.6, R-2.5.9
func TestApplySchema_CurrentGenerationWithAnotherShapeIsNotAGenerationMismatch(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", SqliteFileURI(filepath.Join(t.TempDir(), "state.db"), nil))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	_, err = db.ExecContext(t.Context(), CanonicalSchemaSQLForTest()+`; CREATE TABLE future_table (path TEXT)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), sqlEnsureStoreMetadataRow, currentSyncStoreGeneration)
	require.NoError(t, err)

	err = applySchema(t.Context(), db)
	require.ErrorIs(t, err, errIncompatibleSchema)
	var generationErr *storeGenerationError
	assert.NotErrorAs(t, err, &generationErr)
}
