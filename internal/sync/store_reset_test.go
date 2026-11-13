package sync

import (
	"context"
	"database/sql"
	"net/url"
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

// staleMarkerTable is the table a walDonor keeps only in its log.
const staleMarkerTable = "stale_marker"

// walDonor creates a WAL-mode database in dir whose one table lives only in
// its write-ahead log (no checkpoint has run), and returns the database's
// path. The donor stays open until the test ends, so its files can be copied
// as an interrupted writer would leave them.
func walDonor(t *testing.T, dir string) string {
	t.Helper()

	donorPath := filepath.Join(dir, "donor.db")
	donor, err := sql.Open(sqliteDriverName,
		sqliteFileURI(donorPath, url.Values{sqlitePragmaParam: {"journal_mode(WAL)", "wal_autocheckpoint(0)"}}))
	require.NoError(t, err)
	donor.SetMaxOpenConns(1)
	t.Cleanup(func() {
		assert.NoError(t, donor.Close())
	})
	_, err = donor.ExecContext(t.Context(), "CREATE TABLE "+staleMarkerTable+" (x INTEGER)")
	require.NoError(t, err)

	return donorPath
}

// copyStateFile copies one non-empty file of a test's own temp tree.
func copyStateFile(t *testing.T, from, to string) {
	t.Helper()

	info, err := os.Stat(from)
	require.NoError(t, err)
	require.Positive(t, info.Size())
	require.NoError(t, copyFileContent(from, to))
}

// sqliteHasTable reports whether db has a table called name.
func sqliteHasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count))

	return count == 1
}

// openPlainSQLite opens the database at dbPath as SQLite does by default,
// without the store's schema or pragmas.
func openPlainSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := sql.Open(sqliteDriverName, sqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	return db
}

// SQLite pairs a write-ahead log with a database by file name alone, and the
// rules for a store's file family rest on what it then does
// (config.StateDBFamily, moveStateDBFamily). A log left without its database
// is deleted when a database is created at that name, so a store created
// where an older delete left sidecars starts empty. A log beside a database
// that did not write it is replayed into that database, which is why a new
// account email change clears the sidecars at each target before it is
// recorded (MoveStateDBs), and why a resumed one keeps the log it already
// moved. The control shows that the copied log is one SQLite replays when it
// can.
//
// Validates: R-2.1.3
func TestStateDBFamily_SQLitePairsALogWithADatabaseByNameAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	donorPath := walDonor(t, dir)

	t.Run("a log without its database is deleted, not replayed", func(t *testing.T) {
		dbPath := filepath.Join(dir, "created.db")
		copyStateFile(t, donorPath+"-wal", dbPath+"-wal")

		store, openErr := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
		require.NoError(t, openErr)
		t.Cleanup(func() {
			assert.NoError(t, store.Close(context.Background()))
		})

		assert.False(t, sqliteHasTable(t, store.rawDB(), staleMarkerTable))
		assert.True(t, sqliteHasTable(t, store.rawDB(), "baseline"))
	})

	t.Run("control: a log beside its own database is replayed", func(t *testing.T) {
		dbPath := filepath.Join(dir, "own.db")
		copyStateFile(t, donorPath, dbPath)
		copyStateFile(t, donorPath+"-wal", dbPath+"-wal")

		assert.True(t, sqliteHasTable(t, openPlainSQLite(t, dbPath), staleMarkerTable))
	})

	t.Run("a log beside another database is replayed into it", func(t *testing.T) {
		dbPath := filepath.Join(dir, "other.db")
		store, openErr := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
		require.NoError(t, openErr)
		require.NoError(t, store.Close(context.Background()))
		copyStateFile(t, donorPath+"-wal", dbPath+"-wal")

		db := openPlainSQLite(t, dbPath)
		assert.True(t, sqliteHasTable(t, db, staleMarkerTable))
		assert.False(t, sqliteHasTable(t, db, "baseline"))
	})
}

// A family move cut short after the log moved and before the database did
// leaves the log at the new name and the database at the old one. The rerun
// finds no log at the source; it must finish the move with the log it already
// moved, not delete it as a stranger's: the log holds committed sync state the
// database does not have yet.
//
// Validates: R-3.7.3, R-2.1.3
func TestMoveStateDBFamily_ResumeAfterSidecarsMovedKeepsTheLog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	donorPath := walDonor(t, dir)
	from := filepath.Join(dir, "old.db")
	to := filepath.Join(dir, "new.db")
	// What the interrupted move left: the log renamed, the database not yet.
	copyStateFile(t, donorPath, from)
	copyStateFile(t, donorPath+"-wal", to+"-wal")

	require.NoError(t, moveStateDBFamily(from, to))

	assert.NoFileExists(t, from)
	assert.True(t, sqliteHasTable(t, openPlainSQLite(t, to), staleMarkerTable),
		"the state committed to the log must survive the resumed move")
}
