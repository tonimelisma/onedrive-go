package sync

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walDonor creates a WAL-mode database in dir whose one table lives only in
// its write-ahead log (no checkpoint has run), and returns the database's
// path. The donor stays open until the test ends, so its files can be copied
// as an interrupted writer would leave them.
func walDonor(t *testing.T, dir string) string {
	t.Helper()

	donorPath := filepath.Join(dir, "donor.db")
	donor, err := sql.Open(SqliteDriverName,
		SqliteFileURI(donorPath, url.Values{SqlitePragmaParam: {"journal_mode(WAL)", "wal_autocheckpoint(0)"}}))
	require.NoError(t, err)
	donor.SetMaxOpenConns(1)
	t.Cleanup(func() {
		assert.NoError(t, donor.Close())
	})
	_, err = donor.ExecContext(t.Context(), "CREATE TABLE "+staleMarkerTable+" (x INTEGER)")
	require.NoError(t, err)

	return donorPath
}

// openPlainSQLite opens the database at dbPath as SQLite does by default,
// without the store's schema or pragmas.
func openPlainSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := sql.Open(SqliteDriverName, SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	return db
}
