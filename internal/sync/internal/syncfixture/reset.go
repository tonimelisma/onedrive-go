package syncfixture

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// StaleMarkerTable is the table a walDonor keeps only in its log.
const StaleMarkerTable = "stale_marker"

// CopyStateFile copies one non-empty file of a test's own temp tree.
func CopyStateFile(t *testing.T, from, to string) {
	t.Helper()

	info, err := os.Stat(from)
	require.NoError(t, err)
	require.Positive(t, info.Size())
	require.NoError(t, CopyFileContent(from, to))
}

// SqliteHasTable reports whether db has a table called name.
func SqliteHasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count))

	return count == 1
}
