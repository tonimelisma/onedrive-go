package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3
func TestResetStateDB_RecreatesFreshCanonicalStore(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "bad:name.txt",
		DriveID:   driveid.New(testDriveID),
		IssueType: IssueInvalidFilename,
	})
	require.NoError(t, store.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           SKService(),
		TrialInterval: time.Minute,
		NextTrialAt:   store.nowFunc().Add(time.Minute),
	}))
	require.NoError(t, store.Close(context.Background()))

	require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("stale wal"), 0o600))
	require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("stale shm"), 0o600))

	require.NoError(t, ResetStateDB(t.Context(), dbPath, newTestLogger(t)))

	reopened, err := openSyncStore(t.Context(), dbPath, newTestLogger(t), false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	issues, err := reopened.ListObservationIssues(t.Context())
	require.NoError(t, err)
	assert.Empty(t, issues)

	scopes, err := reopened.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes)

	_, err = os.Stat(dbPath)
	require.NoError(t, err)
}

// A rollback journal left beside a deleted store (a crash while the store
// was first created, before it switched to WAL) belongs to the old file; the
// reset removes it with the rest of the family instead of leaving it for the
// fresh store.
//
// Validates: R-2.1.3
func TestRemoveStateDBFiles_RemovesALeftoverJournal(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a database"), 0o600))
	require.NoError(t, os.WriteFile(dbPath+"-journal", []byte("journal of the old file"), 0o600))

	require.NoError(t, removeStateDBFiles(dbPath))

	assert.NoFileExists(t, dbPath+"-journal")
	assert.NoFileExists(t, dbPath)
}

// SQLite pairs a write-ahead log with a database by file name alone, and the
// rules for a store's file family rest on what it then does
// (config.StateDBFamily, moveStateDBFamily). A log left without its database
// is deleted when a database is created at that name, so a store created
// where an older delete left sidecars starts empty. A log beside a database
// that did not write it is replayed into that database, which is why a move
// clears the sidecars at its target. The control shows that the copied log is
// one SQLite replays when it can.
//
// Validates: R-2.1.3
func TestStateDBFamily_SQLitePairsALogWithADatabaseByNameAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	donorPath := filepath.Join(dir, "donor.db")
	donor, err := sql.Open(sqliteDriverName,
		"file:"+donorPath+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	require.NoError(t, err)
	donor.SetMaxOpenConns(1)
	t.Cleanup(func() {
		assert.NoError(t, donor.Close())
	})
	// With no checkpoint the table lives only in the donor's log.
	_, err = donor.ExecContext(t.Context(), "CREATE TABLE stale_marker (x INTEGER)")
	require.NoError(t, err)

	copyFile := func(t *testing.T, from, to string) {
		t.Helper()

		data, readErr := os.ReadFile(from) //nolint:gosec // test reads its own temp tree
		require.NoError(t, readErr)
		require.NotEmpty(t, data)
		require.NoError(t, os.WriteFile(to, data, 0o600)) //nolint:gosec // test writes its own temp tree
	}
	hasTable := func(t *testing.T, db *sql.DB, name string) bool {
		t.Helper()

		var count int
		require.NoError(t, db.QueryRowContext(t.Context(),
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count))

		return count == 1
	}
	openPlain := func(t *testing.T, dbPath string) *sql.DB {
		t.Helper()

		db, openErr := sql.Open(sqliteDriverName, "file:"+dbPath)
		require.NoError(t, openErr)
		t.Cleanup(func() {
			assert.NoError(t, db.Close())
		})

		return db
	}

	t.Run("a log without its database is deleted, not replayed", func(t *testing.T) {
		dbPath := filepath.Join(dir, "created.db")
		copyFile(t, donorPath+"-wal", dbPath+"-wal")

		store, openErr := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
		require.NoError(t, openErr)
		t.Cleanup(func() {
			assert.NoError(t, store.Close(context.Background()))
		})

		assert.False(t, hasTable(t, store.rawDB(), "stale_marker"))
		assert.True(t, hasTable(t, store.rawDB(), "baseline"))
	})

	t.Run("control: a log beside its own database is replayed", func(t *testing.T) {
		dbPath := filepath.Join(dir, "own.db")
		copyFile(t, donorPath, dbPath)
		copyFile(t, donorPath+"-wal", dbPath+"-wal")

		assert.True(t, hasTable(t, openPlain(t, dbPath), "stale_marker"))
	})

	t.Run("a log beside another database is replayed into it", func(t *testing.T) {
		dbPath := filepath.Join(dir, "other.db")
		store, openErr := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
		require.NoError(t, openErr)
		require.NoError(t, store.Close(context.Background()))
		copyFile(t, donorPath+"-wal", dbPath+"-wal")

		db := openPlain(t, dbPath)
		assert.True(t, hasTable(t, db, "stale_marker"))
		assert.False(t, hasTable(t, db, "baseline"))
	})
}
