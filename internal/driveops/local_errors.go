package driveops

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// LocalErrorKind names what a failed local filesystem operation means for the
// work around it, independent of which call returned it. It is the single
// errno-to-domain table shared by the sync result classifier and CLI error
// presentation, so a read-only mount or a full disk is recognized the same way
// whether a download, a mkdir or a rename hit it.
type LocalErrorKind int

const (
	// LocalErrorOther is any failure this table does not recognize.
	LocalErrorOther LocalErrorKind = iota
	// LocalErrorNoSpace covers the min_free_space reserve (ErrDiskFull), a
	// full disk (ENOSPC, which is also what an exhausted inode table
	// returns) and an exhausted user quota (EDQUOT). Every write on the
	// filesystem fails the same way, so it is a disk-wide condition.
	LocalErrorNoSpace
	// LocalErrorReadOnly is EROFS: the filesystem holding the path is mounted
	// read-only. It is not os.ErrPermission (only EACCES/EPERM are), so before
	// this table it was retried file by file as an unexpected failure.
	LocalErrorReadOnly
	// LocalErrorFileTooLarge is EFBIG: the file is larger than the local
	// filesystem can store (4 GiB on FAT32). Retrying cannot help.
	LocalErrorFileTooLarge
	// LocalErrorBusy is EBUSY, ETXTBSY or ESTALE: the path is in use, is a
	// running executable, or is a stale network-filesystem handle. Each
	// clears on its own, so the item is retried.
	LocalErrorBusy
	// LocalErrorCrossDevice is EXDEV: a rename tried to cross a mount point
	// inside the sync folder.
	LocalErrorCrossDevice
	// LocalErrorNameTooLong is ENAMETOOLONG: OneDrive accepts names longer
	// than the 255 bytes a Linux or macOS filesystem can store.
	LocalErrorNameTooLong
)

// ClassifyLocalError maps a local filesystem error chain to its domain kind.
// It branches on sentinels and errnos only, never on text.
func ClassifyLocalError(err error) LocalErrorKind {
	if err == nil {
		return LocalErrorOther
	}

	switch {
	case errors.Is(err, ErrDiskFull), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return LocalErrorNoSpace
	case errors.Is(err, syscall.EROFS):
		return LocalErrorReadOnly
	case errors.Is(err, syscall.EFBIG):
		return LocalErrorFileTooLarge
	case errors.Is(err, syscall.EBUSY), errors.Is(err, syscall.ETXTBSY), errors.Is(err, syscall.ESTALE):
		return LocalErrorBusy
	case errors.Is(err, syscall.EXDEV):
		return LocalErrorCrossDevice
	case errors.Is(err, syscall.ENAMETOOLONG):
		return LocalErrorNameTooLong
	default:
		return LocalErrorOther
	}
}

// wrapLocalWriteError adds the disk-full sentinel and a plain cause to a
// write failure that ran out of space, so callers that only branch on
// ErrDiskFull (and users reading the message) see a full disk rather than a
// raw "no space left on device". An exhausted inode table also returns
// ENOSPC while bytes remain free, which the min_free_space pre-check cannot
// see; statfs tells the two apart. dir is the directory the write targeted.
// Other errors are returned unchanged.
func wrapLocalWriteError(dir string, err error) error {
	if err == nil || errors.Is(err, ErrDiskFull) {
		return err
	}

	switch {
	case errors.Is(err, syscall.EDQUOT):
		return fmt.Errorf("%w: your disk quota on this computer is used up: %w", ErrDiskFull, err)
	case errors.Is(err, syscall.ENOSPC):
		if inodesExhausted(dir) {
			return fmt.Errorf("%w: the filesystem has no free inodes (too many files) although bytes are free: %w",
				ErrDiskFull, err)
		}
		return fmt.Errorf("%w: no space left on the local disk: %w", ErrDiskFull, err)
	default:
		return err
	}
}

// inodesExhausted reports whether the filesystem holding dir has no free
// inodes. Filesystems with dynamic inode allocation (btrfs, ZFS) report a
// zero total, which is not exhaustion. A statfs failure reports false: the
// caller still says the disk is full, just without naming inodes.
func inodesExhausted(dir string) bool {
	if dir == "" {
		dir = string(os.PathSeparator)
	}

	free, total, err := inodeCounts(dir)
	return err == nil && total > 0 && free == 0
}
