package sync

import (
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
