//go:build darwin || linux

// disk_unix.go — Disk space availability for Unix-like systems.
//
// Uses syscall.Statfs to query available disk space. f_bavail (blocks
// available to unprivileged users) is used instead of f_bfree which
// includes reserved blocks.
package sync

import "syscall"

// diskAvailable returns the number of bytes available to unprivileged users
// on the filesystem containing path. Uses statfs(2) which is supported on
// both Darwin and Linux.
func diskAvailable(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// f_bavail = blocks available to unprivileged users
	// f_bsize = fundamental block size
	return stat.Bavail * uint64(stat.Bsize), nil
}
