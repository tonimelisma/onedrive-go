package synctree

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameExclusiveAt is renameat2 with RENAME_NOREPLACE: it fails with EEXIST
// instead of replacing to. refused reports a kernel or filesystem that does
// not offer the flag (ENOSYS, EINVAL), so the portable fallback has to move
// the entry.
func renameExclusiveAt(fromDir int, from string, toDir int, to string) (bool, error) {
	err := unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return true, err //nolint:wrapcheck // the caller wraps with both names
	}

	return false, err //nolint:wrapcheck // the caller wraps with both names
}
