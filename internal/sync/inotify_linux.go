//go:build linux

package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// inotifyCapacityThreshold is the fraction of max_user_watches that triggers
// a capacity warning. At 80% usage, the operator should increase the limit.
const inotifyCapacityThreshold = 0.8

// percentMultiplier converts a fraction (0.0–1.0) to a percentage (0–100).
const percentMultiplier = 100.0

// readInotifyLimit reads the current inotify max_user_watches from procfs.
// Returns 0 if procfs is unavailable (e.g., container without /proc mounted).
func readInotifyLimit() (int, error) {
	data, err := localpath.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("sync: reading inotify limit: %w", err)
	}

	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("sync: parsing inotify limit: %w", err)
	}

	return limit, nil
}

// checkInotifyCapacity warns if the estimated directory count exceeds 80% of
// the inotify max_user_watches limit and points operators at the sysctl knob
// they need to raise before watch startup becomes fragile.
func checkInotifyCapacity(estimatedDirs int, logger *slog.Logger) {
	// Read the current procfs value directly instead of caching it in global
	// process state. This path runs at observer startup and the extra read is
	// negligible compared with the clarity of having no package-level state.
	limit, err := readInotifyLimit()
	if err != nil {
		logger.Debug("failed to read inotify watch limit",
			slog.String("error", err.Error()))

		return
	}

	if limit == 0 {
		return
	}

	usage := float64(estimatedDirs) / float64(limit)
	if usage >= inotifyCapacityThreshold {
		logger.Warn(fmt.Sprintf("Watching about %d folders is close to the system limit (%d). "+
			"Raise it with: sudo sysctl -w fs.inotify.max_user_watches=524288", estimatedDirs, limit),
			slog.Int("estimated_dirs", estimatedDirs),
			slog.Int("max_user_watches", limit),
			slog.String("usage", fmt.Sprintf("%.0f%%", usage*percentMultiplier)),
			slog.String("advice", "sudo sysctl -w fs.inotify.max_user_watches=524288"),
		)
	}
}

// watchCreateEMFILECause: on Linux, EMFILE from inotify_init1 usually means
// fs.inotify.max_user_instances is exhausted (other inotify users such as
// editors and IDEs share it); the per-process open-file limit is the other
// possibility, and the warning names both.
const watchCreateEMFILECause = watchCauseInstanceLimit

// watchDescriptorBudget is zero on Linux: an inotify watch is a kernel watch
// descriptor counted against max_user_watches, not an open file.
func watchDescriptorBudget() int { return 0 }

// remoteFilesystemMagics names the statfs f_type values of network and FUSE
// filesystems, whose changes made by other clients raise no inotify events.
var remoteFilesystemMagics = map[uint32]string{
	unix.NFS_SUPER_MAGIC:  "nfs",
	unix.SMB_SUPER_MAGIC:  "smb",
	unix.CIFS_SUPER_MAGIC: "cifs",
	unix.SMB2_SUPER_MAGIC: "smb2",
	unix.FUSE_SUPER_MAGIC: "fuse",
	unix.V9FS_MAGIC:       "9p",
	unix.CEPH_SUPER_MAGIC: "ceph",
	unix.AFS_SUPER_MAGIC:  "afs",
	unix.CODA_SUPER_MAGIC: "coda",
}

// localFilesystemKind identifies the filesystem holding path from statfs.
func localFilesystemKind(path string) (filesystemInfo, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return filesystemInfo{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	// f_type is int64 on amd64 and int32 on 32-bit platforms; the magic
	// numbers are 32-bit, so compare the low 32 bits.
	if kind, ok := remoteFilesystemMagics[uint32(stat.Type)]; ok { //nolint:gosec // truncation to the 32-bit magic is intended
		return filesystemInfo{kind: kind, remote: true}, nil
	}

	return filesystemInfo{kind: fmt.Sprintf("0x%x", stat.Type)}, nil
}
