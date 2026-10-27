package sync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func seedUnavailableRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFolder("item-dir", "root", "Dir")
	remote.addFile(t, "item-d1", "item-dir", "one.txt", "one\n")
	remote.addFile(t, "item-d2", "item-dir", "two.txt", "two\n")

	return remote
}

func requireUnavailableIssue(t *testing.T, eng *testEngine, path string) {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	for i := range issues {
		if issues[i].Path == path && issues[i].IssueType == issueLocalUnsyncableReplacement {
			return
		}
	}
	require.Failf(t, "missing issue", "no %s issue at %s in %+v", issueLocalUnsyncableReplacement, path, issues)
}

// Validates: R-6.2.14
func TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		mutate     func(t *testing.T, syncRoot string)
		syncedPath string
		filter     ContentFilterConfig
	}{
		{
			name: "file renamed to a name OneDrive refuses",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "a:b.txt")))
			},
			syncedPath: "a.txt",
		},
		{
			name: "populated folder renamed to a trailing-dot name",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "Dir"), filepath.Join(syncRoot, "Dir.")))
			},
			syncedPath: "Dir",
		},
		{
			name: "folder replaced by a symbolic link to outside the sync folder",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				outside := filepath.Join(filepath.Dir(syncRoot), "outside")
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "Dir"), outside))
				require.NoError(t, os.Symlink(outside, filepath.Join(syncRoot, "Dir")))
			},
			syncedPath: "Dir",
		},
		{
			name: "file replaced by a FIFO",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(syncRoot, "a.txt")))
				require.NoError(t, syscall.Mkfifo(filepath.Join(syncRoot, "a.txt"), 0o600))
			},
			syncedPath: "a.txt",
		},
		{
			name: "file renamed into an excluded dotfile name",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, ".a.txt")))
			},
			syncedPath: "a.txt",
			filter:     ContentFilterConfig{IgnoreDotfiles: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := seedUnavailableRemote(t)
			eng, syncRoot := newTestEngine(t, remote.client(t))
			eng.contentFilter = tc.filter
			eng.execCfg.SetContentFilter(tc.filter)
			runScenarioPass(t, eng)

			tc.mutate(t, syncRoot)
			report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)

			assert.Empty(t, remote.deletes, "an unrepresentable synced item must not be deleted on OneDrive")
			assert.Zero(t, report.RemoteDeletes)
			requireUnavailableIssue(t, eng, tc.syncedPath)
		})
	}
}

// failingInfoDirEntry is a directory entry whose stat fails the way a dying
// disk or a stale network handle does.
type failingInfoDirEntry struct{ name string }

func (d failingInfoDirEntry) Name() string               { return d.name }
func (d failingInfoDirEntry) IsDir() bool                { return false }
func (d failingInfoDirEntry) Type() fs.FileMode          { return 0 }
func (d failingInfoDirEntry) Info() (fs.FileInfo, error) { return nil, syscall.EIO }

// Validates: R-6.2.13
func TestFullScan_EntryLstatEIOIsReadBoundaryNotDelete(t *testing.T) {
	t.Parallel()

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	var (
		events  []changeEvent
		jobs    []hashJob
		skipped []skippedItem
	)

	err := obs.processEntry("/sync/a.txt", "a.txt", "a.txt", failingInfoDirEntry{name: "a.txt"},
		map[string]bool{}, map[string]localStateRow{}, &events, &jobs, &skipped, 0)

	require.NoError(t, err)
	require.Len(t, skipped, 1)
	assert.Equal(t, "a.txt", skipped[0].Path)
	assert.Equal(t, issueLocalReadDenied, skipped[0].Reason)
	assert.True(t, skipped[0].BlocksReadBoundary, "an unreadable entry is unknown, not absent")
}

// Validates: R-6.2.13
func TestFullScan_HashEIOProducesNoUploadAndNoDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "content")
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = func(string) (string, error) {
		return "", &fs.PathError{Op: "read", Path: "a.txt", Err: syscall.EIO}
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	var issue *skippedItem
	for i := range result.Skipped {
		if result.Skipped[i].Path == "a.txt" {
			issue = &result.Skipped[i]
		}
	}
	require.NotNil(t, issue, "a read failure must be recorded, making the path unavailable truth")
	assert.Equal(t, issueHashPanic, issue.Reason)
	assert.True(t, observationIssueBlocksLocalTruth(issue.Reason))
}

// Validates: R-6.2.15
func TestIsForeignMountPoint_DoesNotCrossFilesystemsByDefault(t *testing.T) {
	t.Parallel()

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	availability := newScanAvailability(10)

	assert.False(t, obs.isForeignMountPoint(availability, "Docs", synctree.FileIdentity{Device: 10, Inode: 2}, true))
	assert.True(t, obs.isForeignMountPoint(availability, "Docs/nas", synctree.FileIdentity{Device: 11, Inode: 3}, true),
		"a folder on another device is a mount point sync does not cross")

	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	assert.False(t, obs.isForeignMountPoint(availability, "Docs/usb", synctree.FileIdentity{Device: 12, Inode: 4}, true),
		"cross_filesystems opts in")

	item := mountBoundarySkippedItem("Docs/nas")
	assert.True(t, item.BlocksReadBoundary, "everything beneath an uncrossed mount stays unavailable")
	assert.Equal(t, issueLocalMountBoundary, item.Reason)
}

// A disk mounted inside the sync folder was crossed under cross_filesystems
// and synced; later it is not mounted, leaving an empty folder on the parent
// filesystem. Three files are far below the delete-safety hold, so only the
// recorded mount boundary keeps them from being deleted on OneDrive (D-003).
//
// Validates: R-6.2.15
func TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-nas", "root", "nas")
	for i := range 3 {
		remote.addFile(t, fmt.Sprintf("item-n%d", i), "item-nas", fmt.Sprintf("n%d.txt", i), "nas file\n")
	}
	eng, syncRoot := newTestEngine(t, remote.client(t))
	filter := ContentFilterConfig{CrossFilesystems: true}
	eng.contentFilter = filter
	runScenarioPass(t, eng)

	// The scan that crossed into the disk recorded the mount point.
	_, err := eng.baseline.rawDB().ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES ('nas')`)
	require.NoError(t, err)

	// The disk is not mounted: the mount point is an empty folder now.
	for i := range 3 {
		require.NoError(t, os.Remove(filepath.Join(syncRoot, "nas", fmt.Sprintf("n%d.txt", i))))
	}

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Empty(t, remote.deletes, "an unmounted disk's synced files must not be deleted on OneDrive")
	assert.Zero(t, report.RemoteDeletes)
	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	found := false
	for i := range issues {
		found = found || (issues[i].Path == "nas" && issues[i].IssueType == issueLocalMountBoundary)
	}
	assert.True(t, found, "the vanished mount is reported: %+v", issues)
}

// Validates: R-6.2.15
func TestIsForeignMountPoint_CrossingRecordsTheBoundary(t *testing.T) {
	t.Parallel()

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	availability := newScanAvailability(10)

	assert.False(t, obs.isForeignMountPoint(availability, "nas", synctree.FileIdentity{Device: 11, Inode: 3}, true))
	assert.Equal(t, []string{"nas"}, availability.crossedMounts)

	obs.SetMountBoundaryFlags([]string{"usb"})
	assert.False(t, obs.isForeignMountPoint(availability, "usb", synctree.FileIdentity{Device: 10, Inode: 4}, true))
	assert.True(t, obs.flaggedMountIsGone(availability, "usb"), "a flagged mount point back on its parent's device is not mounted")
}

// Validates: R-6.2.13
func TestRootDeviceForScan_UnreadableRootAbortsScan(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.Mkdir(dir, 0o750))
	tree := mustOpenSyncTree(t, dir)
	require.NoError(t, os.Remove(dir))

	_, err := rootDeviceForScan(tree)
	require.ErrorIs(t, err, ErrMountRootUnavailable)
}

// A user who empties a disk that is still mounted is deleting files: only a
// flagged folder back on its parent's device is an unmounted disk.
//
// Validates: R-6.2.15
func TestFlaggedMountIsGone_MountedButEmptiedDiskIsNotABoundary(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(
		&BaselineEntry{Path: "nas", ItemID: "nas", ItemType: ItemTypeFolder},
		&BaselineEntry{Path: "nas/a.txt", ItemID: "a", ItemType: ItemTypeFile},
	)
	obs := newLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	obs.SetMountBoundaryFlags([]string{"nas"})
	availability := newScanAvailability(10)
	availability.tree = mustOpenSyncTree(t, t.TempDir())
	require.NoError(t, os.Mkdir(filepath.Join(availability.tree.Path(), "nas"), 0o750))

	assert.False(t, obs.isForeignMountPoint(availability, "nas", synctree.FileIdentity{Device: 11, Inode: 3}, true))
	assert.False(t, obs.flaggedMountIsGone(availability, "nas"),
		"the disk is mounted (its own device); its emptied content is deleted like any other")
}

// Validates: R-6.2.15
func TestClearSyncRootIdentity_ForgetsRecordedMountPoints(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	_, err := eng.baseline.rawDB().ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES ('nas')`)
	require.NoError(t, err)

	require.NoError(t, eng.baseline.ClearSyncRootIdentity(t.Context()))

	flags, err := eng.baseline.listMountBoundaries(t.Context())
	require.NoError(t, err)
	assert.Empty(t, flags, "a rebind accepts the folder as it is now")
	assert.Contains(t, vanishedMountSkippedItem("nas").Detail, "drive rebind-root")
}

// Validates: R-6.4.9
func TestContentFilter_RestoreCopiesAreNeverObserved(t *testing.T) {
	t.Parallel()

	filter := newContentFilter(ContentFilterConfig{})
	assert.False(t, filter.Visible("Docs/.onedrive-go-restore-123", ItemTypeFile))
	assert.False(t, filter.Visible("Docs/.onedrive-go.0123456789abcdef.detach", ItemTypeFile))
}

// Validates: R-6.2.12
func TestCountSyncFolderFiles_MissingFolderHoldsNoFiles(t *testing.T) {
	t.Parallel()

	count, err := CountSyncFolderFiles(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	assert.Zero(t, count)
}
