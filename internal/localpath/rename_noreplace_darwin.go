package localpath

import (
	"errors"

	"golang.org/x/sys/unix"
)

func renameNoReplace(src, dst string) error {
	err := unix.RenamexNp(src, dst, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return linkOrCopyNoReplace(src, dst, defaultNoReplaceOps())
	}
	return err //nolint:wrapcheck // the caller wraps with both paths
}
