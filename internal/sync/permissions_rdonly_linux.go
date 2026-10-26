//go:build linux

package sync

import "golang.org/x/sys/unix"

// mountIsReadOnly reads the read-only flag of the mount holding absDir from
// statfs f_flags. Linux fills f_flags from the mount itself, so a read-only
// bind mount of a writable filesystem (`mount --bind -o ro`) reports
// ST_RDONLY even though its superblock is writable, which access(W_OK) can
// miss. known is false when statfs fails.
func mountIsReadOnly(absDir string) (readOnly, known bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(absDir, &stat); err != nil {
		return false, false
	}

	return stat.Flags&unix.ST_RDONLY != 0, true
}
