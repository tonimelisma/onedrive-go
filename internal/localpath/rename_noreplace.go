package localpath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrTargetExists reports that RenameNoReplace found something at the
// destination and left both paths as they were.
var ErrTargetExists = errors.New("destination already exists")

// ErrSourceNotRemoved reports that RenameNoReplace put the file at the
// destination but could not remove the source. The move itself succeeded;
// callers treat it as success and report the leftover.
var ErrSourceNotRemoved = errors.New("the file is in place but the original could not be removed")

// RenameNoReplace renames src to dst only if dst does not exist. It is
// atomic where the platform can be (renameat2 RENAME_NOREPLACE on Linux,
// renamex_np RENAME_EXCL on macOS); otherwise it links then unlinks, and on
// filesystems without hard links it creates dst with O_EXCL, copies, syncs and
// removes src. That last fallback is not atomic (a crash can leave both
// files), but it never replaces anything at dst. A cross-filesystem rename
// fails with an error matching syscall.EXDEV.
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
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return fmt.Errorf("renaming %s to %s: %w", src, dst, ErrTargetExists)
	default:
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}
}

// noReplaceOps are the filesystem operations of the portable fallback; a
// seam so tests can model filesystems without hard links.
type noReplaceOps struct {
	link     func(src, dst string) error
	remove   func(path string) error
	copyExcl func(src, dst string) error
}

func defaultNoReplaceOps() noReplaceOps {
	return noReplaceOps{link: os.Link, remove: os.Remove, copyExcl: copyExclusive}
}

// linkOrCopyNoReplace is the portable no-replace rename: link fails with
// EEXIST instead of replacing; where the filesystem has no hard links, an
// O_EXCL create plus copy does the same job without atomicity.
func linkOrCopyNoReplace(src, dst string, ops noReplaceOps) error {
	err := ops.link(src, dst)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrExist):
		return err
	case linkUnsupported(err):
		if copyErr := ops.copyExcl(src, dst); copyErr != nil {
			return copyErr
		}
	default:
		return err
	}

	if removeErr := ops.remove(src); removeErr != nil {
		return fmt.Errorf("%w: %w", ErrSourceNotRemoved, removeErr)
	}
	return nil
}

func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EMLINK)
}

// copyExclusive creates dst (failing if it exists), copies src into it with
// src's mode, and syncs it. A failed copy removes the partial dst.
func copyExclusive(src, dst string) (err error) {
	in, err := OpenRegular(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only source

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stating %s: %w", src, err)
	}
	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer func() {
		if err != nil {
			_ = out.Close()    //nolint:errcheck // error path; the copy error is reported
			_ = os.Remove(dst) //nolint:errcheck // best-effort removal of a partial copy
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s: %w", src, err)
	}
	if err = out.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dst, err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	if err = os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("setting times on %s: %w", dst, err)
	}
	return nil
}
