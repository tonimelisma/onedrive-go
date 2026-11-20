package syncobserve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
	syncfixture.SkipUnlessByteExactNames(t)

	syncRoot := t.TempDir()
	syncfixture.WriteLocalFile(t, syncRoot, filepath.Join(syncfixture.NfdCafe+" dir", syncfixture.NfdCafe+".txt"), "x")
	tree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	stored := syncfixture.NfcCafe + " dir/" + syncfixture.NfcCafe + ".txt"
	fsPath := filepath.Join(syncRoot, syncfixture.NfdCafe+" dir", syncfixture.NfdCafe+".txt")

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
	syncfixture.SkipUnlessByteExactNames(t)

	dir := t.TempDir()
	onDisk := filepath.Join(dir, syncfixture.NfdCafe+" dir")
	require.NoError(t, os.Mkdir(onDisk, 0o700))
	watcher := newAddErrWatcher(nil, nil)
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.activeWatcher = watcher

	obs.rewatchUnwatchedDirectories(syncfixture.MustOpenSyncTree(t, dir), []syncdomain.LocalStateRow{{Path: syncfixture.NfcCafe + " dir", ItemType: syncdomain.ItemTypeFolder}})

	assert.Contains(t, watcher.addedPaths(), onDisk)
	assert.NotContains(t, watcher.addedPaths(), filepath.Join(dir, syncfixture.NfcCafe+" dir"))
}
