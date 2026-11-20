package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.13.2
//
// Watch-mode re-checks look an NFD-named file up by its spelling on disk: a
// delete event for it while it still exists (an atomic save) is not a
// delete, and its synced path is not gone.
func TestWatchRechecks_FindTheNFDNamedLocalFile(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	syncRoot := t.TempDir()
	writeLocalFile(t, syncRoot, filepath.Join(nfdCafe+" dir", nfdCafe+".txt"), "x")
	tree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	stored := nfcCafe + " dir/" + nfcCafe + ".txt"
	fsPath := filepath.Join(syncRoot, nfdCafe+" dir", nfdCafe+".txt")

	assert.True(t, localPathStillPresent(tree, stored, fsPath), "the event's file still exists")
	assert.False(t, syncedPathGone(tree, stored), "the synced file is still there")

	require.NoError(t, os.Remove(fsPath))
	assert.False(t, localPathStillPresent(tree, stored, fsPath))
	assert.True(t, syncedPathGone(tree, stored))
}

// Validates: R-2.13.2, R-2.8.13
//
// A full scan's rows carry the stored NFC path; the watch on an NFD-named
// folder is added on its spelling on disk. Adding the NFC spelling failed
// with ENOENT on every scan and left the folder unwatched.
func TestRewatchUnwatchedDirectories_WatchesTheNFDNamedFolder(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	dir := t.TempDir()
	onDisk := filepath.Join(dir, nfdCafe+" dir")
	require.NoError(t, os.Mkdir(onDisk, 0o700))
	watcher := newAddErrWatcher(nil, nil)
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.activeWatcher = watcher

	obs.rewatchUnwatchedDirectories(mustOpenSyncTree(t, dir), []LocalStateRow{{Path: nfcCafe + " dir", ItemType: ItemTypeFolder}})

	assert.Contains(t, watcher.addedPaths(), onDisk)
	assert.NotContains(t, watcher.addedPaths(), filepath.Join(dir, nfcCafe+" dir"))
}
