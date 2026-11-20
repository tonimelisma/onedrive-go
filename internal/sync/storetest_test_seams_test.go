package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireStoreTestRawDB(t *testing.T, store *SyncStore) *sql.DB {
	t.Helper()

	db := store.RawDBForTest()
	require.NotNil(t, db)

	return db
}
