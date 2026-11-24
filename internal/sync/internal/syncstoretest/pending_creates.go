package syncstoretest

import (
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func ReadBaselineRow(t *testing.T, mgr *syncstore.SyncStore, itemID string) (path string, localSize int64, found bool) {
	t.Helper()

	err := RawDB(t, mgr).QueryRowContext(t.Context(),
		`SELECT path, coalesce(local_size, -1) FROM baseline WHERE item_id = ?`, itemID).Scan(&path, &localSize)
	if err != nil {
		return "", 0, false
	}

	return path, localSize, true
}
