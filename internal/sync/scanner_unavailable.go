package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Unavailable is not absent. A full scan builds local truth from what it
// could represent; everything else used to be simply missing from that
// truth, and a synced path that is missing reads as a delete. These helpers
// turn each "could not represent" case into an observation issue on the
// synced path, which the planner treats as unavailable local truth (no
// remote delete, and for a folder none beneath it):
//
//   - a synced path now occupied by a symlink sync does not follow, or by a
//     FIFO, socket or device (D-004);
//   - a synced item renamed to a name OneDrive refuses or a path too long, or
//     into an excluded name such as a dotfile (D-004, P-5), recognized by its
//     filesystem identity reappearing under the skipped name;
//   - a folder that is a mount point for another filesystem, which sync does
//     not cross by default (D-003, P-11). The boundary is a read boundary, so
//     whatever the baseline holds beneath it stays unavailable, and an
//     unmounted disk leaves an empty folder that never held synced content.
//
// The scan is single-threaded through the walk, so the per-scan state below
// is owned by FullScan for its duration.

// scanAvailability is the per-scan state the unavailability checks need.
type scanAvailability struct {
	// dirDevices maps a scanned folder's relative path ("" for the root) to
	// its device, so a child folder on a different device is a mount point.
	dirDevices map[string]uint64
	// baselineByIdentity indexes baseline rows by filesystem identity, built
	// on first use, so a skipped entry can be matched to the synced item it
	// used to be.
	baselineByIdentity map[synctree.FileIdentity]*BaselineEntry
	// crossedMounts lists mount points crossed under cross_filesystems;
	// the engine records them with the snapshot (mount_boundaries).
	crossedMounts []string
	// tree is the scanned sync tree, for emptiness checks.
	tree *synctree.Root
}

func newScanAvailability(rootDevice uint64) *scanAvailability {
	return &scanAvailability{dirDevices: map[string]uint64{"": rootDevice}}
}

// rootDeviceForScan is the device of the sync folder itself. Without it no
// mount boundary below the root can be recognised, so a failure stops the
// scan rather than letting it cross into other filesystems unnoticed.
func rootDeviceForScan(tree *synctree.Root) (uint64, error) {
	identity, err := tree.RootIdentity()
	if err != nil {
		return 0, fmt.Errorf("%w: reading the sync folder's device: %w", ErrMountRootUnavailable, err)
	}
	return identity.Device, nil
}

func (a *scanAvailability) baselineEntryWithIdentity(baseline *Baseline, identity synctree.FileIdentity) *BaselineEntry {
	if a.baselineByIdentity == nil {
		a.baselineByIdentity = make(map[synctree.FileIdentity]*BaselineEntry)
		if baseline != nil {
			baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
				if entry == nil || !entry.LocalHasIdentity {
					return
				}
				a.baselineByIdentity[synctree.FileIdentity{Device: entry.LocalDevice, Inode: entry.LocalInode}] = entry
			})
		}
	}
	return a.baselineByIdentity[identity]
}

// recordSyncedPathReplaced keeps a synced path that now holds an entry sync
// skips (a symlink it does not follow, a special file) unavailable instead
// of deleted.
func (o *localObserver) recordSyncedPathReplaced(skipped *[]skippedItem, dbRelPath string, what string) {
	entry, ok := o.baseline.GetByPath(dbRelPath)
	if !ok {
		return
	}
	*skipped = append(*skipped, skippedItem{
		Path:   dbRelPath,
		Reason: issueLocalUnsyncableReplacement,
		Detail: fmt.Sprintf("this synced %s was replaced by %s, which sync does not sync; the OneDrive copy is kept",
			itemNoun(entry.ItemType), what),
		BlocksReadBoundary: entry.ItemType == ItemTypeFolder,
	})
}

// recordSyncedItemMovedOutOfReach keeps a synced item that was renamed to
// something sync skips (an invalid name, a too-long path, an excluded name)
// unavailable at its synced path, recognized by its filesystem identity.
func (o *localObserver) recordSyncedItemMovedOutOfReach(
	skipped *[]skippedItem,
	availability *scanAvailability,
	dbRelPath string,
	d fs.DirEntry,
) {
	if availability == nil || d == nil {
		return
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok || hasOtherHardLinks(info) {
		// A second hard link is a second name for a file that may still be
		// at its synced path; it is not evidence the synced name went away.
		return
	}
	entry := availability.baselineEntryWithIdentity(o.baseline, identity)
	if entry == nil || entry.Path == dbRelPath {
		return
	}
	*skipped = append(*skipped, skippedItem{
		Path:   entry.Path,
		Reason: issueLocalUnsyncableReplacement,
		Detail: fmt.Sprintf("this synced %s is now %s, which sync cannot sync; the OneDrive copy is kept",
			itemNoun(entry.ItemType), dbRelPath),
		BlocksReadBoundary: entry.ItemType == ItemTypeFolder,
	})
}

// isForeignMountPoint records the folder's device and reports whether the
// folder is the mount point of another filesystem that the scan must not
// cross (P-11). Only current devices are compared (the folder and its parent,
// both read in this scan), because device numbers can change between boots.
// A mount point crossed under cross_filesystems is remembered in the scan
// result, so a later scan can tell its unmounted, empty folder from deleted
// content.
func (o *localObserver) isForeignMountPoint(
	availability *scanAvailability,
	dbRelPath string,
	identity synctree.FileIdentity,
	hasIdentity bool,
) bool {
	if availability == nil || !hasIdentity {
		return false
	}
	parentDevice, known := availability.dirDevices[parentRelPath(dbRelPath)]
	availability.dirDevices[dbRelPath] = identity.Device
	if !known || parentDevice == 0 || identity.Device == parentDevice {
		return false
	}
	if o.filterConfig.CrossFilesystems {
		availability.crossedMounts = append(availability.crossedMounts, dbRelPath)
		return false
	}

	return true
}

// flaggedMountIsGone reports whether a folder recorded as a crossed mount
// point no longer holds that filesystem: it now sits on its parent's device,
// or it is empty while the baseline still holds content beneath it. Either
// way the disk is not mounted, and its synced content is unavailable, not
// deleted (D-003). Call after isForeignMountPoint recorded the folder's
// device.
func (o *localObserver) flaggedMountIsGone(availability *scanAvailability, dbRelPath string) bool {
	if availability == nil {
		return false
	}
	if _, flagged := o.mountBoundaryFlags[dbRelPath]; !flagged {
		return false
	}
	device := availability.dirDevices[dbRelPath]
	parentDevice, known := availability.dirDevices[parentRelPath(dbRelPath)]
	if known && parentDevice != 0 && device == parentDevice {
		return true
	}
	if availability.tree == nil || !o.baselineHasDescendants(dbRelPath) {
		return false
	}
	empty, err := availability.tree.DirEmptyNoFollow(dbRelPath)
	return err == nil && empty
}

func (o *localObserver) baselineHasDescendants(dbRelPath string) bool {
	prefix := dbRelPath + "/"
	found := false
	o.baseline.ForEachPath(func(entryPath string, _ *BaselineEntry) {
		if !found && strings.HasPrefix(entryPath, prefix) {
			found = true
		}
	})
	return found
}

func parentRelPath(dbRelPath string) string {
	parent := path.Dir(dbRelPath)
	if parent == "." {
		return ""
	}
	return parent
}

func vanishedMountSkippedItem(dbRelPath string) skippedItem {
	return skippedItem{
		Path:   dbRelPath,
		Reason: issueLocalMountBoundary,
		Detail: "this folder was a mount point for another filesystem that is no longer mounted; " +
			"its synced content is kept until the disk is back",
		BlocksReadBoundary: true,
	}
}

func mountBoundarySkippedItem(dbRelPath string) skippedItem {
	return skippedItem{
		Path:   dbRelPath,
		Reason: issueLocalMountBoundary,
		Detail: "this folder is a mount point for another filesystem; sync does not cross into it " +
			"(set cross_filesystems = true to sync it)",
		BlocksReadBoundary: true,
	}
}

// recordUnreadableEntry turns an entry the walk listed but could not stat
// into a read boundary. Only "it vanished" is absence; an I/O error is not
// (D-005).
func recordUnreadableEntry(skipped *[]skippedItem, dbRelPath string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	*skipped = append(*skipped, skippedItem{
		Path:               dbRelPath,
		Reason:             issueLocalReadDenied,
		Detail:             fmt.Sprintf("could not read this entry (%v); it is kept as it was", err),
		BlocksReadBoundary: true,
	})
}

func hasOtherHardLinks(info fs.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}

func itemNoun(itemType ItemType) string {
	if itemType == ItemTypeFolder {
		return strFolder
	}
	return strFile
}
