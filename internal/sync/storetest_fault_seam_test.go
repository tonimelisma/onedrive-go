package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// registerFaultDriver registers the fault driver under faultDriverName, once
// per test binary.
func registerFaultDriver(t *testing.T) {
	t.Helper()

	faultDriverOnce.Do(func() {
		probe, err := sql.Open(SqliteDriverName, ":memory:")
		require.NoError(t, err)
		inner := probe.Driver()
		require.NoError(t, probe.Close())
		sql.Register(faultDriverName, &faultDriver{inner: inner})
	})
}
