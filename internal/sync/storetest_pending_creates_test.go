package sync

import (
	"testing"
)

func readBaselineRow(t *testing.T, mgr *SyncStore, itemID string) (path string, localSize int64, found bool) {
	t.Helper()

	err := mgr.RawDBForTest().QueryRowContext(t.Context(),
		`SELECT path, coalesce(local_size, -1) FROM baseline WHERE item_id = ?`, itemID).Scan(&path, &localSize)
	if err != nil {
		return "", 0, false
	}

	return path, localSize, true
}
