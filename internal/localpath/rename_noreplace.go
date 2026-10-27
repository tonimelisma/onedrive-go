package localpath

import (
	"errors"
	"fmt"
	"os"
)

// ErrTargetExists reports that RenameNoReplace found something at the
// destination and left both paths as they were.
var ErrTargetExists = errors.New("destination already exists")

// RenameNoReplace renames src to dst only if dst does not exist, atomically
// where the platform can (renameat2 RENAME_NOREPLACE on Linux, renamex_np
// RENAME_EXCL on macOS) and with a hard link then unlink elsewhere. A
// cross-filesystem rename fails with an error matching syscall.EXDEV.
func RenameNoReplace(src, dst string) error {
	absSrc, err := absolutePath(src)
	if err != nil {
		return err
	}
	absDst, err := absolutePath(dst)
	if err != nil {
		return err
	}

	err = renameNoReplace(absSrc, absDst)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, ErrTargetExists)
	}
	if err != nil {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}

	return nil
}

// linkThenUnlink is the portable no-replace rename for regular files: link
// fails with EEXIST instead of replacing.
func linkThenUnlink(src, dst string) error {
	//nolint:forbidigo // localpath is the arbitrary-path boundary
	if err := os.Link(src, dst); err != nil {
		return err //nolint:wrapcheck // the caller wraps with both paths
	}
	//nolint:forbidigo // localpath is the arbitrary-path boundary
	return os.Remove(src) //nolint:wrapcheck // the caller wraps with both paths
}
