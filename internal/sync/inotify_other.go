//go:build !linux

package sync

import (
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/unix"
)

// readInotifyLimit is a no-op on non-Linux platforms. fsnotify uses kqueue on
// macOS and the BSDs (it has no FSEvents backend), and kqueue has no
// inotify-style max_user_watches counter to read. Its real limit is different:
// kqueue holds an open file descriptor for every watched directory and every
// file inside one, so a large tree is bounded by the process's open-file limit.
func readInotifyLimit() (int, error) { return 0, nil }

// checkInotifyCapacity is a no-op on non-Linux platforms. fsnotify uses
// kqueue on macOS and the BSDs, which has no inotify-style watch limit but
// holds one open descriptor per watched directory and file; that budget is
// checked separately.
func checkInotifyCapacity(_ int, _ *slog.Logger) {}

// watchCreateEMFILECause: without inotify, EMFILE from watcher creation is the
// open-file limit.
const watchCreateEMFILECause = watchCauseFileLimit

// watchDescriptorFraction is the share of the open-file limit watch setup may
// commit to kqueue watches. The rest stays for hashing, transfers, SQLite and
// the network.
const watchDescriptorFraction = 2

// maxWatchDescriptorBudget bounds the budget when the limit is effectively
// unlimited, so the arithmetic stays in int range.
const maxWatchDescriptorBudget = 1 << 30

// watchDescriptorBudget is half of the process's open-file limit: fsnotify's
// kqueue backend opens a descriptor for every watched directory and every
// file inside one (D-079). It returns 0 (no budget) when the limit cannot be
// read, leaving EMFILE handling as the backstop.
func watchDescriptorBudget() int {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return 0
	}

	current := uint64(limit.Cur) //nolint:unconvert,nolintlint // Cur is uint64 on darwin but int64 on freebsd
	budget := current / watchDescriptorFraction
	if budget == 0 || budget > maxWatchDescriptorBudget {
		budget = maxWatchDescriptorBudget
	}

	return int(budget)
}

// isRemoteFilesystemType reports whether a statfs f_fstypename names a
// network or FUSE filesystem, whose changes made by other clients raise no
// kqueue events.
func isRemoteFilesystemType(kind string) bool {
	switch kind {
	case "nfs", "smbfs", "afpfs", "webdav", "cifs", "macfuse", "osxfuse", "fusefs":
		return true
	case "fuseblk":
		// Block-backed FUSE drivers for local disks see every change.
		return false
	default:
		return strings.HasPrefix(kind, "fuse")
	}
}

// localFilesystemKind identifies the filesystem holding path from statfs.
func localFilesystemKind(path string) (filesystemInfo, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return filesystemInfo{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	kind := unix.ByteSliceToString(stat.Fstypename[:])
	return filesystemInfo{kind: kind, remote: isRemoteFilesystemType(kind)}, nil
}
