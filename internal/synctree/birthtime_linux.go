//go:build linux

package synctree

import (
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// statBirthTime is zero on Linux: stat(2) has no birth time field, so
// withBirthTime reads it with statx(2) instead.
func statBirthTime(*syscall.Stat_t) int64 {
	return 0
}

// birthTimeInfo is a FileInfo carrying the birth time statx reported.
type birthTimeInfo struct {
	fs.FileInfo
	birthTime int64
}

func (i birthTimeInfo) BirthTime() (int64, bool) {
	return i.birthTime, i.birthTime != 0
}

// withBirthTime returns info carrying the entry's birth time, read with
// statx(2) STATX_BTIME at absPath. It is used where the sync tree stats an
// entry for its identity (rooted Stat and Lstat, directory walks and
// listings) and by watch-mode observation, so both record one identity.
//
// Not every filesystem reports a birth time: ext4 (with 256-byte inodes, the
// default), xfs v5, btrfs, f2fs and tmpfs (Linux 5.x and later) set the mask
// bit; ext2/3, vfat/exfat, NFS, CIFS, overlayfs over a filesystem without it
// and most FUSE filesystems do not, and a kernel older than 4.11 has no statx
// at all. Any of those leaves info as it is, so its identity has no birth
// time and falls back to (device, inode): the documented fallback, not a
// failure, which is why the statx error is not returned.
//
// statx resolves absPath again after the stat that produced info, so the
// result is used only when it names the same device and inode; an entry
// replaced in between keeps info's identity without a birth time.
func withBirthTime(info fs.FileInfo, absPath string) fs.FileInfo {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return info
	}
	// info describes a symbolic link itself only when it came from an
	// lstat; otherwise following the final component reaches the same
	// entry info describes.
	flags := unix.AT_STATX_DONT_SYNC
	if info.Mode()&fs.ModeSymlink != 0 {
		flags |= unix.AT_SYMLINK_NOFOLLOW
	}
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, absPath, flags, unix.STATX_INO|unix.STATX_BTIME, &stx); err != nil {
		return info
	}

	return birthTimeFromStatx(info, stat, &stx)
}

// withDescriptorBirthTime is withBirthTime for an entry the caller holds open:
// statx(2) reads the descriptor itself (AT_EMPTY_PATH), so the birth time is
// the open directory's even if its path now names another one. The same
// fallback applies: no birth time, info unchanged.
func withDescriptorBirthTime(info fs.FileInfo, file *os.File) fs.FileInfo {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return info
	}
	var stx unix.Statx_t
	err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC,
		unix.STATX_INO|unix.STATX_BTIME, &stx)
	if err != nil {
		return info
	}

	return birthTimeFromStatx(info, stat, &stx)
}

// birthTimeFromStatx attaches stx's birth time to info when statx reported
// one for the same device and inode info describes.
func birthTimeFromStatx(info fs.FileInfo, stat *syscall.Stat_t, stx *unix.Statx_t) fs.FileInfo {
	if stx.Mask&unix.STATX_BTIME == 0 || stx.Mask&unix.STATX_INO == 0 {
		return info
	}
	if stx.Ino != stat.Ino || unix.Mkdev(stx.Dev_major, stx.Dev_minor) != stat.Dev {
		return info
	}
	birthTime := stx.Btime.Sec*int64(time.Second) + int64(stx.Btime.Nsec)
	if birthTime == 0 {
		return info
	}

	return birthTimeInfo{FileInfo: info, birthTime: birthTime}
}
