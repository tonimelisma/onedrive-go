//go:build linux

package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
			slog.Any("error", err))

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

// remoteFilesystemName names the statfs f_type magic of a network or FUSE
// filesystem, whose changes made by other clients raise no inotify events.
func remoteFilesystemName(magic uint32) (string, bool) {
	switch magic {
	case unix.NFS_SUPER_MAGIC:
		return "nfs", true
	case unix.SMB_SUPER_MAGIC:
		return "smb", true
	case unix.CIFS_SUPER_MAGIC:
		return "cifs", true
	case unix.SMB2_SUPER_MAGIC:
		return "smb2", true
	case unix.FUSE_SUPER_MAGIC:
		return "fuse", true
	case unix.V9FS_MAGIC:
		return "9p", true
	case unix.CEPH_SUPER_MAGIC:
		return "ceph", true
	case unix.AFS_SUPER_MAGIC:
		return "afs", true
	case unix.CODA_SUPER_MAGIC:
		return "coda", true
	default:
		return "", false
	}
}

// localFilesystemKind identifies the filesystem holding path from statfs.
func localFilesystemKind(path string) (filesystemInfo, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return filesystemInfo{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	// f_type is int64 on amd64 and int32 on 32-bit platforms; the magic
	// numbers are 32-bit, so compare the low 32 bits.
	kind, ok := remoteFilesystemName(uint32(stat.Type))
	if !ok {
		return filesystemInfo{kind: fmt.Sprintf("0x%x", stat.Type)}, nil
	}
	if uint32(stat.Type) != unix.FUSE_SUPER_MAGIC {
		return filesystemInfo{kind: kind, remote: true}, nil
	}

	// FUSE covers both servers that synthesize content (sshfs, rclone
	// mount) and block-backed drivers for local disks (ntfs-3g, exfat-fuse),
	// which mount as "fuseblk" and see every change through this kernel.
	// statfs cannot tell them apart; the mount table can.
	mountinfo, err := localpath.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return filesystemInfo{kind: kind, remote: true}, nil //nolint:nilerr // unknown FUSE flavor: assume the conservative case
	}
	if fsType, found := mountFilesystemType(string(mountinfo), path); found {
		return filesystemInfo{kind: fsType, remote: fsType != "fuseblk"}, nil
	}

	return filesystemInfo{kind: kind, remote: true}, nil
}

// mountFilesystemType returns the filesystem type of the mount that holds
// path, from /proc/self/mountinfo text: the mount whose mount point is the
// longest prefix of path.
func mountFilesystemType(mountinfo, path string) (string, bool) {
	path = filepath.Clean(path)
	bestLen := -1
	bestType := ""
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if len(fields) < 5 || sep < 0 || sep+1 >= len(fields) {
			continue
		}
		mountPoint := unescapeMountinfo(fields[4])
		if !pathWithin(path, mountPoint) || len(mountPoint) <= bestLen {
			continue
		}
		bestLen = len(mountPoint)
		bestType = fields[sep+1]
	}

	return bestType, bestLen >= 0
}

func pathWithin(path, mountPoint string) bool {
	if mountPoint == "/" || path == mountPoint {
		return true
	}

	return strings.HasPrefix(path, mountPoint+"/")
}

// unescapeMountinfo decodes the octal escapes (\040 for a space) the kernel
// writes for special characters in mountinfo paths.
func unescapeMountinfo(field string) string {
	if !strings.Contains(field, "\\") {
		return field
	}

	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if v, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}

	return out.String()
}
