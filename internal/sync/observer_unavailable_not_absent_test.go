package sync

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// failingInfoDirEntry is a directory entry whose stat fails the way a dying
// disk or a stale network handle does.
type failingInfoDirEntry struct{ name string }

func (d failingInfoDirEntry) Name() string { return d.name }

func (d failingInfoDirEntry) IsDir() bool { return false }

func (d failingInfoDirEntry) Type() fs.FileMode { return 0 }

func (d failingInfoDirEntry) Info() (fs.FileInfo, error) { return nil, syscall.EIO }

// Validates: R-6.2.13
func TestFullScan_EntryLstatEIOIsReadBoundaryNotDelete(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	var (
		events  []ChangeEvent
		jobs    []hashJob
		skipped []SkippedItem
	)

	err := obs.processEntry("/sync/a.txt", "a.txt", failingInfoDirEntry{name: "a.txt"},
		map[string]bool{}, map[string]LocalStateRow{}, &events, &jobs, &skipped, 0)

	require.NoError(t, err)
	require.Len(t, skipped, 1)
	assert.Equal(t, "a.txt", skipped[0].Path)
	assert.Equal(t, IssueLocalReadDenied, skipped[0].Reason)
	assert.True(t, skipped[0].BlocksReadBoundary, "an unreadable entry is unknown, not absent")
}

// Validates: R-6.2.13
func TestFullScan_HashEIOProducesNoUploadAndNoDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "content")
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = func(context.Context, string) (string, error) {
		return "", &fs.PathError{Op: "read", Path: "a.txt", Err: syscall.EIO}
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	var issue *SkippedItem
	for i := range result.Skipped {
		if result.Skipped[i].Path == "a.txt" {
			issue = &result.Skipped[i]
		}
	}
	require.NotNil(t, issue, "a read failure must be recorded, making the path unavailable truth")
	assert.Equal(t, IssueHashPanic, issue.Reason)
	assert.True(t, ObservationIssueBlocksLocalTruth(issue.Reason))
}

// Validates: R-6.2.15
func TestIsForeignMountPoint_DoesNotCrossFilesystemsByDefault(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	availability := newScanAvailability(10)

	assert.False(t, obs.isForeignMountPoint(availability, "Docs", synctree.FileIdentity{Device: 10, Inode: 2}, true))
	assert.True(t, obs.isForeignMountPoint(availability, "Docs/nas", synctree.FileIdentity{Device: 11, Inode: 3}, true),
		"a folder on another device is a mount point sync does not cross")

	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	assert.False(t, obs.isForeignMountPoint(availability, "Docs/usb", synctree.FileIdentity{Device: 12, Inode: 4}, true),
		"cross_filesystems opts in")

	item := MountBoundarySkippedItem("Docs/nas")
	assert.True(t, item.BlocksReadBoundary, "everything beneath an uncrossed mount stays unavailable")
	assert.Equal(t, IssueLocalMountBoundary, item.Reason)
}

// Validates: R-6.2.15
func TestIsForeignMountPoint_CrossingRecordsTheBoundary(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	availability := newScanAvailability(10)

	assert.False(t, obs.isForeignMountPoint(availability, "nas", synctree.FileIdentity{Device: 11, Inode: 3}, true))
	assert.Equal(t, []string{"nas"}, availability.crossedMounts)

	// A scan copies the recorded flags into its availability when it starts.
	obs.SetMountBoundaryFlags([]string{"usb"})
	availability.mountBoundaryFlags = *obs.mountBoundaryFlags.Load()
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
	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{CrossFilesystems: true})
	obs.SetMountBoundaryFlags([]string{"nas"})
	availability := newScanAvailability(10)
	availability.tree = mustOpenSyncTree(t, t.TempDir())
	require.NoError(t, os.Mkdir(filepath.Join(availability.tree.Path(), "nas"), 0o750))

	assert.False(t, obs.isForeignMountPoint(availability, "nas", synctree.FileIdentity{Device: 11, Inode: 3}, true))
	assert.False(t, obs.flaggedMountIsGone(availability, "nas"),
		"the disk is mounted (its own device); its emptied content is deleted like any other")
}
