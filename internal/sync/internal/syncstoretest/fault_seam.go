package syncstoretest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// RegisterFaultDriver registers the fault driver under faultDriverName, once
// per test binary.
func RegisterFaultDriver(t *testing.T) {
	t.Helper()

	syncfixture.FaultDriverOnce.Do(func() {
		probe, err := sql.Open(syncstore.SqliteDriverName, ":memory:")
		require.NoError(t, err)
		inner := probe.Driver()
		require.NoError(t, probe.Close())
		sql.Register(syncfixture.FaultDriverName, &syncfixture.FaultDriver{Inner: inner})
	})
}
