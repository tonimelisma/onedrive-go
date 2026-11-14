package synctree

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameExclusiveAt is renameatx_np with RENAME_EXCL: it fails with EEXIST
// instead of replacing to. refused reports a filesystem that does not offer
// the flag (ENOTSUP, EINVAL), so the portable fallback has to move the entry.
func renameExclusiveAt(fromDir int, from string, toDir int, to string) (bool, error) {
	err := unix.RenameatxNp(fromDir, from, toDir, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return true, err //nolint:wrapcheck // the caller wraps with both names
	}

	return false, err //nolint:wrapcheck // the caller wraps with both names
}
