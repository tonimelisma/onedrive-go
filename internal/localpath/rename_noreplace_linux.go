package localpath

import (
	"errors"

	"golang.org/x/sys/unix"
)

func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		// Old kernels and some filesystems do not support the flag.
		return linkOrCopyNoReplace(src, dst, defaultNoReplaceOps())
	}
	return err //nolint:wrapcheck // the caller wraps with both paths
}
