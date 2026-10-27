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

	current := uint64(limit.Cur) //nolint:gosec,unconvert // Cur is uint64 on darwin, int64 on freebsd; both are non-negative limits
	budget := current / watchDescriptorFraction
	if budget == 0 || budget > maxWatchDescriptorBudget {
		budget = maxWatchDescriptorBudget
	}

	return int(budget) //nolint:gosec // bounded by maxWatchDescriptorBudget above
}

// remoteFilesystemTypes names the statfs f_fstypename values of network and
// FUSE filesystems, whose changes made by other clients raise no kqueue
// events.
var remoteFilesystemTypes = map[string]bool{
	"nfs":     true,
	"smbfs":   true,
	"afpfs":   true,
	"webdav":  true,
	"cifs":    true,
	"macfuse": true,
	"osxfuse": true,
	"fusefs":  true,
}

// localFilesystemKind identifies the filesystem holding path from statfs.
func localFilesystemKind(path string) (filesystemInfo, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return filesystemInfo{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	var name strings.Builder
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		name.WriteByte(byte(c))
	}
	kind := name.String()
	remote := remoteFilesystemTypes[kind] || strings.HasPrefix(kind, "fuse")

	return filesystemInfo{kind: kind, remote: remote}, nil
}
