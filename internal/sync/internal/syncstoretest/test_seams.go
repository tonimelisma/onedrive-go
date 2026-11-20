package syncstoretest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func RequireStoreTestRawDB(t *testing.T, store *syncstore.SyncStore) *sql.DB {
	t.Helper()

	db := store.RawDBForTest()
	require.NotNil(t, db)

	return db
}
