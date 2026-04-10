package syncstore

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func newMigrationProvider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()

	migrations, err := fs.Sub(migrationFS, "migrations")
	require.NoError(t, err)

	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations,
		goose.WithLogger(goose.NopLogger()),
		goose.WithDisableGlobalRegistry(true),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, provider.Close())
	})

	return provider
}

// Validates: R-2.5.6
func TestSyncStore_MigrationProviderFreshDBUpgradesToCurrent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	provider := newMigrationProvider(t, db)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)

	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	assert.Equal(t, currentMigrationVersion, version)
}

// Validates: R-2.5.6, R-2.3.6, R-2.3.12
func TestSyncStore_MigrationFromVersionOnePreservesDurableIntentSemantics(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	provider := newMigrationProvider(t, db)

	_, err = provider.UpTo(t.Context(), 1)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution, state, requested_resolution, requested_at, resolving_at, resolution_error, resolved_at, resolved_by)
		VALUES
			('conflict-unresolved', ?, 'item-a', '/a.txt', 'edit_edit', 1, 'unresolved', 'unresolved', NULL, NULL, NULL, NULL, NULL, NULL),
			('conflict-queued', ?, 'item-b', '/b.txt', 'edit_edit', 2, 'unresolved', 'resolution_requested', 'keep_local', 20, NULL, NULL, NULL, NULL),
			('conflict-failed', ?, 'item-c', '/c.txt', 'edit_edit', 3, 'unresolved', 'resolve_failed', 'keep_remote', 30, NULL, 'boom', NULL, NULL),
			('conflict-resolved', ?, 'item-d', '/d.txt', 'edit_edit', 4, 'keep_both', 'resolved', NULL, NULL, NULL, NULL, 40, 'user');
		INSERT INTO held_deletes
			(drive_id, action_type, path, item_id, state, held_at, approved_at, last_planned_at)
		VALUES
			(?, 'remote_delete', '/delete-me.txt', 'item-delete', 'approved', 10, 11, 12)`,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), version)

	require.NoError(t, db.Close())

	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	queued, err := store.GetConflictRequest(t.Context(), "conflict-queued")
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateResolutionRequested, queued.State)
	assert.Equal(t, synctypes.ResolutionKeepLocal, queued.RequestedResolution)
	assert.Equal(t, int64(20), queued.RequestedAt)

	failed, err := store.GetConflictRequest(t.Context(), "conflict-failed")
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateResolveFailed, failed.State)
	assert.Equal(t, synctypes.ResolutionKeepRemote, failed.RequestedResolution)
	assert.Equal(t, "boom", failed.ResolutionError)

	resolved, err := store.GetConflict(t.Context(), "conflict-resolved")
	require.NoError(t, err)
	assert.Equal(t, synctypes.ResolutionKeepBoth, resolved.Resolution)
	assert.Equal(t, int64(40), resolved.ResolvedAt)
	assert.Equal(t, synctypes.ResolvedByUser, resolved.ResolvedBy)

	approved, err := store.ListHeldDeletesByState(t.Context(), synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "item-delete", approved[0].ItemID)
}

// Validates: R-2.5.6
func TestSyncStore_MigrationFilesIncludeUpDownAndMatchCurrentVersion(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(migrationFS, "migrations")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	var maxVersion int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		raw, readErr := fs.ReadFile(migrationFS, filepath.Join("migrations", name))
		require.NoError(t, readErr)

		content := string(raw)
		assert.Contains(t, content, "-- +goose Up", name)
		assert.Contains(t, content, "-- +goose Down", name)

		prefix, _, found := strings.Cut(name, "_")
		require.True(t, found, name)
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		require.NoError(t, parseErr)
		if version > maxVersion {
			maxVersion = version
		}
	}

	assert.Equal(t, currentMigrationVersion, maxVersion)
}
