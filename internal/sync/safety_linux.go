//go:build linux

package sync

import "golang.org/x/sys/unix"

// getDiskSpace returns available bytes on the volume containing path.
// Uses unix.Statfs instead of syscall.Statfs because the syscall package
// has inconsistent field types across architectures. The unix package
// normalizes this. Uses Bavail (available to unprivileged users), not
// Bfree (total free including root-reserved blocks).
func getDiskSpace(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}

	// Bsize is int64 on Linux — always non-negative from the kernel.
	bsize := uint64(stat.Bsize) // kernel guarantees non-negative block size

	return stat.Bavail * bsize, nil
}
