package sync

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setStoreTestNow(store *SyncStore, now time.Time) {
	store.setNowFunc(func() time.Time { return now })
}

func requireStoreTestExec(t *testing.T, store *SyncStore, query string, args ...any) {
	t.Helper()

	_, err := store.rawDB().ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func requireStoreTestRawDB(t *testing.T, store *SyncStore) *sql.DB {
	t.Helper()

	db := store.rawDB()
	require.NotNil(t, db)

	return db
}
