package syncstoretest

import (
	"database/sql"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// rawDBs holds the database of every store a test opened with WithRawDB's
// hooks, by store, for RawDB. The store hands its database to no importer;
// only a test that opened it through these hooks can reach its tables. An
// entry leaves with the test that opened the store (tb.Cleanup).
//
//nolint:gochecknoglobals // a store cannot carry its test's handle to its own database; tests run in parallel.
var rawDBs stdsync.Map

// WithRawDB is hooks with the store's database kept for RawDB, for the test
// tb, which opens the store.
func WithRawDB(tb testing.TB, hooks syncstore.Hooks) syncstore.Hooks {
	tb.Helper()

	hooks.OnOpen = func(store *syncstore.SyncStore, db *sql.DB) {
		rawDBs.Store(store, db)
		tb.Cleanup(func() { rawDBs.Delete(store) })
	}

	return hooks
}

// RawDB is the database of store, which the test opened with WithRawDB's
// hooks (every store syncstoretest and the engine test helpers open), for a
// test that reads or seeds tables below the typed API.
func RawDB(tb testing.TB, store *syncstore.SyncStore) *sql.DB {
	tb.Helper()

	kept, found := rawDBs.Load(store)
	require.True(tb, found, "the store was not opened with syncstoretest.WithRawDB's hooks")
	db, isDB := kept.(*sql.DB)
	require.True(tb, isDB, "rawDBs holds databases only")

	return db
}
