//go:build unix

// Package driveops handles OneDrive transfer and local filesystem operations.
package driveops

import (
	"fmt"
	"syscall"
)

// statfsFieldToUint64 normalizes a syscall.Statfs_t numeric field to uint64.
// Bavail and Bsize differ in signedness across Unix platforms (uint64/int64 on
// linux and darwin, int64 on freebsd), so the conversion is necessary on every
// target rather than redundant on some.
func statfsFieldToUint64[T ~int32 | ~uint32 | ~int64 | ~uint64](value T) uint64 {
	return uint64(value)
}

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
	return statfsFieldToUint64(stat.Bavail) * statfsFieldToUint64(stat.Bsize), nil
}
