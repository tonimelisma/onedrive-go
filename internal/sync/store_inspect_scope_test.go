package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.48
//
// A drive synced with included_dirs still observes the whole drive's change
// list, so remote_state holds every item; status counted all of them as
// pending remote changes forever (6,345 on a drive limited to one folder,
// fault round of 2026-11-03). The count applies the drive's filter.
func TestReadDriveStatusSnapshot_RemoteDriftCountsOnlyTheSyncScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	_, err := store.rawDB().ExecContext(t.Context(), `
		INSERT OR REPLACE INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-in-dir', 'faults', 'folder', NULL, NULL, 1700000000),
			('item-in', 'faults/a.txt', 'file', 'abc', 3, 1700000000),
			('item-out-dir', 'other', 'folder', NULL, NULL, 1700000000),
			('item-out', 'other/b.txt', 'file', 'def', 3, 1700000000)`)
	require.NoError(t, err)
	dbPath := syncStorePathForStoreScopeTest(t, store)

	whole, err := ReadDriveStatusSnapshot(t.Context(), dbPath, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 4, whole.RemoteDriftItems, "without a filter every new item is pending")

	scoped, err := ReadDriveStatusSnapshotWithFilter(t.Context(), dbPath, testLogger(t), ContentFilterConfig{IncludedDirs: []string{"faults"}})
	require.NoError(t, err)
	assert.Equal(t, 2, scoped.RemoteDriftItems, "only the included folder and its file count")
}
