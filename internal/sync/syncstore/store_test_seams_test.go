package syncstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setStoreTestNow(store *SyncStore, now time.Time) {
	store.SetNowFunc(func() time.Time { return now })
}

func requireStoreTestExec(t *testing.T, store *SyncStore, query string, args ...any) {
	t.Helper()

	_, err := store.RawDBForTest().ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}
