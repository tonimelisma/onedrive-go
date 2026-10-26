//go:build darwin || freebsd

package sync

import "golang.org/x/sys/unix"

// mountIsReadOnly reads MNT_RDONLY of the mount holding absDir from
// statfs f_flags. known is false when statfs fails.
func mountIsReadOnly(absDir string) (readOnly, known bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(absDir, &stat); err != nil {
		return false, false
	}

	return stat.Flags&unix.MNT_RDONLY != 0, true
}
