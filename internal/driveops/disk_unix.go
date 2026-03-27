// Package driveops handles OneDrive transfer and local filesystem operations.
package driveops

import (
	"fmt"
	"syscall"
)

// DiskAvailable returns the number of bytes available to unprivileged users
// on the filesystem containing path. Uses statfs(2) which is supported on
// both Darwin and Linux. It uses f_bavail rather than f_bfree so the result
// matches what an unprivileged sync process can actually allocate.
func DiskAvailable(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	// f_bavail = blocks available to unprivileged users
	// f_bsize = fundamental block size
	return stat.Bavail * uint64(stat.Bsize), nil
}
