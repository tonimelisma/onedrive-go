//go:build darwin

package sync

import "syscall"

// getDiskSpace returns available bytes on the volume containing path.
// Uses Bavail (available to unprivileged users), not Bfree (total free
// including root-reserved blocks).
func getDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}

	return stat.Bavail * uint64(stat.Bsize), nil
}
