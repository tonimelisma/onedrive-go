package syncstore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.16
//
// A side type the enum does not know means the SQL and the Go vocabulary
// drifted apart: the scan fails and names the path.
func TestQueryComparisonRows_UnknownSideTypeFailsTheScanNamingThePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	_, err := queryComparisonRows(t.Context(), store.RawDBForTest(),
		`SELECT 'docs/compared.txt', '', '', 0, 0, 0, '', '', 0, '', '',
			'file', 'bogus', 'file', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)
	require.ErrorContains(t, err, `"bogus"`)
}
