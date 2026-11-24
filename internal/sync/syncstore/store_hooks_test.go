package syncstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A store opened with zero hooks behaves as production does: it opens
// through the SQLite driver, and its crash points do nothing.
//
// Validates: R-6.10.29
func TestHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SqliteDriverName, Hooks{}.driverName(), "the store opens through SQLite")

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenEngineSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), nil, Hooks{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.WithoutCancel(t.Context()))) })

	for _, point := range syncfixture.AllCrashPoints() {
		require.NoError(t, store.reachCrashPoint(point), "crash point %s does nothing", point)
	}
	assert.Equal(t, sqliteSynchronousFull, storeSynchronous(t, store), "the store commits with synchronous=FULL")

	plain, err := NewSyncStore(synctest.TestContext(t), filepath.Join(t.TempDir(), "plain.db"), synctest.TestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, plain.Close(context.WithoutCancel(t.Context()))) })
	assert.Equal(t, sqliteSynchronousFull, storeSynchronous(t, plain), "a store opened without hooks commits with synchronous=FULL")
}

// SkipFsync, the tests' setting, is the only way to a store that does not
// wait for the disk.
//
// Validates: R-6.10.29
func TestHooks_SkipFsyncTurnsSynchronousOff(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenEngineSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), nil, Hooks{SkipFsync: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.WithoutCancel(t.Context()))) })

	assert.Equal(t, sqliteSynchronousOff, storeSynchronous(t, store))
}

// OnOpen is told the store as it opens, with its database: the one way a
// test reaches the store's tables. The store itself hands its database to no
// importer.
//
// Validates: R-6.10.29
func TestHooks_OnOpenIsToldTheStoreAndItsDatabase(t *testing.T) {
	t.Parallel()

	var (
		toldStore *SyncStore
		toldDB    *sql.DB
	)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenEngineSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), nil,
		Hooks{OnOpen: func(opened *SyncStore, db *sql.DB) { toldStore, toldDB = opened, db }})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.WithoutCancel(t.Context()))) })

	assert.Same(t, store, toldStore)
	assert.Same(t, store.db, toldDB)
}

// SQLite's PRAGMA synchronous values.
const (
	sqliteSynchronousOff  = 0
	sqliteSynchronousFull = 2
)

// storeSynchronous is the synchronous setting of store's connection.
func storeSynchronous(t *testing.T, store *SyncStore) int {
	t.Helper()

	var synchronous int
	require.NoError(t, store.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))

	return synchronous
}
