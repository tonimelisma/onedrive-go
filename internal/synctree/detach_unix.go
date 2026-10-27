//go:build unix

package synctree

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrDetachRaced reports that the entry at a path stopped being the file that
// was verified before it could be taken out of the tree. Nothing was removed;
// whatever is at the path now is left there.
var ErrDetachRaced = errors.New("the file changed while it was being removed")

// detachTempPrefix and detachTempSuffix give a detached file the client-owned
// partial name pattern, so local observation ignores it for the instant it
// exists inside the tree.
const (
	detachTempPrefix    = ".onedrive-go."
	detachTempSuffix    = ".detach.partial"
	detachRandomBytes   = 8
	detachCopyFilePerm  = 0o600
	detachDirectoryPerm = 0o700
)

// VerifyFunc inspects the open file a destructive operation is about to act
// on. It reads through f, never through the path, so what it approves is
// exactly the inode the operation then removes or preserves.
type VerifyFunc func(f *os.File, info os.FileInfo) error

// DetachRegularFile moves the regular file at rel out of the sync tree to
// dest (an absolute path outside it), but only the inode verify approved.
//
// The old shape, hash by path and then remove by path, unlinked whatever was
// at the path by then: an editor's atomic save during a long hash lost the
// new content (D-010). Here the file is opened once without following links,
// verified through that descriptor, renamed to a temporary sibling inside the
// tree, and the sibling's identity is compared with the descriptor's. Only a
// match leaves the tree; a mismatch is put back and reported as
// ErrDetachRaced. dest lands by rename on the same filesystem and by copy
// through the verified descriptor across filesystems.
func (r *Root) DetachRegularFile(rel string, verify VerifyFunc, dest string) error {
	parent, name, file, info, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	tempName, err := setAsideVerified(parent, name, info, rel)
	if err != nil {
		return err
	}

	if err := moveDetachedOut(parent, tempName, file, dest); err != nil {
		if restoreErr := linkBackNoReplace(parent, tempName, name); restoreErr != nil {
			return fmt.Errorf("%w; putting %s back: %w", err, rel, restoreErr)
		}
		return err
	}

	return nil
}

// RemoveVerifiedRegularFile permanently removes the regular file at rel, but
// only the inode verify approved; see DetachRegularFile for the binding.
func (r *Root) RemoveVerifiedRegularFile(rel string, verify VerifyFunc) error {
	parent, name, file, info, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	tempName, err := setAsideVerified(parent, name, info, rel)
	if err != nil {
		return err
	}

	return withFd(parent, func(fd int) error {
		if unlinkErr := unix.Unlinkat(fd, tempName, 0); unlinkErr != nil {
			return fmt.Errorf("removing %s: %w", rel, unlinkErr)
		}
		return nil
	})
}

// PreserveRegularFile copies the verified regular file at rel to dest without
// taking it out of the tree: a hard link on the same filesystem, a copy
// through the verified descriptor across filesystems. A download that then
// replaces rel by rename leaves the preserved inode untouched.
func (r *Root) PreserveRegularFile(rel string, verify VerifyFunc, dest string) error {
	parent, name, file, info, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	linkErr := linkatOut(parent, name, dest)
	switch {
	case linkErr == nil:
		// The link is by name; prove it names the verified inode.
		linked, statErr := os.Lstat(dest)
		if statErr == nil && os.SameFile(linked, info) {
			return nil
		}
		if removeErr := os.Remove(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("discarding stale copy of %s: %w", rel, removeErr)
		}
		return fmt.Errorf("preserving %s: %w", rel, ErrDetachRaced)
	case errors.Is(linkErr, unix.EXDEV), errors.Is(linkErr, unix.EPERM), errors.Is(linkErr, unix.EMLINK):
		return copyThroughDescriptor(file, dest)
	default:
		return fmt.Errorf("preserving %s: %w", rel, linkErr)
	}
}

func (r *Root) openVerifiedRegular(
	rel string,
	verify VerifyFunc,
) (parent *os.File, name string, file *os.File, info os.FileInfo, err error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, "", nil, nil, err
	}
	if clean == "." {
		return nil, "", nil, nil, unsafePathf("refusing to remove the sync folder itself")
	}
	if err := r.ValidateNoSymlinkAncestors(clean); err != nil {
		return nil, "", nil, nil, err
	}

	parentRel := filepath.Dir(clean)
	name = filepath.Base(clean)
	parent, err = r.Open(parentRel)
	if err != nil {
		return nil, "", nil, nil, err
	}

	file, err = openatNoFollow(parent, name)
	if err != nil {
		_ = parent.Close() //nolint:errcheck // error path; the open failure is reported
		return nil, "", nil, nil, &os.PathError{Op: "open", Path: r.nameForMessage(clean), Err: err}
	}
	info, err = file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = unsupportedEntryf("%s is not a regular file", r.nameForMessage(clean))
	}
	if err == nil && verify != nil {
		err = verify(file, info)
	}
	if err != nil {
		_ = file.Close()   //nolint:errcheck // error path; the verify failure is reported
		_ = parent.Close() //nolint:errcheck // error path; the verify failure is reported
		return nil, "", nil, nil, err
	}

	return parent, name, file, info, nil
}

// setAsideVerified renames name to a temporary sibling and proves the
// sibling is the verified inode. On a mismatch the sibling is put back and
// ErrDetachRaced returned.
func setAsideVerified(parent *os.File, name string, info os.FileInfo, rel string) (string, error) {
	tempName, err := detachTempName()
	if err != nil {
		return "", err
	}
	if err := renameat(parent, name, parent, tempName); err != nil {
		return "", fmt.Errorf("setting aside %s: %w", rel, err)
	}
	if err := sameFileAt(parent, tempName, info); err != nil {
		if restoreErr := linkBackNoReplace(parent, tempName, name); restoreErr != nil {
			return "", fmt.Errorf("%w; putting %s back: %w", err, rel, restoreErr)
		}
		return "", fmt.Errorf("removing %s: %w", rel, err)
	}

	return tempName, nil
}

func detachTempName() (string, error) {
	var random [detachRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("naming a temporary file: %w", err)
	}
	return detachTempPrefix + hex.EncodeToString(random[:]) + detachTempSuffix, nil
}

func withFd(f *os.File, fn func(fd int) error) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("accessing descriptor: %w", err)
	}
	var opErr error
	if ctrlErr := conn.Control(func(fd uintptr) {
		opErr = fn(int(fd)) //nolint:gosec // descriptors fit in int on every unix target
	}); ctrlErr != nil {
		return fmt.Errorf("accessing descriptor: %w", ctrlErr)
	}
	return opErr
}

func openatNoFollow(parent *os.File, name string) (*os.File, error) {
	var fd int
	err := withFd(parent, func(dirfd int) error {
		var openErr error
		fd, openErr = unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		return openErr
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil //nolint:gosec // fd came from openat and is non-negative
}

func renameat(fromDir *os.File, from string, toDir *os.File, to string) error {
	return withFd(fromDir, func(fromFd int) error {
		return withFd(toDir, func(toFd int) error {
			return unix.Renameat(fromFd, from, toFd, to)
		})
	})
}

func sameFileAt(dir *os.File, name string, want os.FileInfo) error {
	var st unix.Stat_t
	if err := withFd(dir, func(fd int) error {
		return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	}); err != nil {
		return fmt.Errorf("checking the set-aside file: %w", err)
	}
	wantID, ok := IdentityFromFileInfo(want)
	if !ok || statFieldToUint64(st.Dev) != wantID.Device || statFieldToUint64(st.Ino) != wantID.Inode {
		return ErrDetachRaced
	}
	return nil
}

// linkBackNoReplace returns a set-aside file to its name without replacing
// anything that appeared there meanwhile: link fails with EEXIST instead of
// overwriting. If the name is taken, the set-aside file stays under its
// temporary name rather than destroying either version.
func linkBackNoReplace(dir *os.File, tempName string, name string) error {
	return withFd(dir, func(fd int) error {
		if err := unix.Linkat(fd, tempName, fd, name, 0); err != nil {
			return fmt.Errorf("restoring %s (kept as %s): %w", name, tempName, err)
		}
		return unix.Unlinkat(fd, tempName, 0)
	})
}

func linkatOut(dir *os.File, name string, dest string) error {
	return withFd(dir, func(fd int) error {
		return unix.Linkat(fd, name, unix.AT_FDCWD, dest, 0)
	})
}

func moveDetachedOut(parent *os.File, tempName string, file *os.File, dest string) error {
	err := withFd(parent, func(fd int) error {
		return unix.Renameat(fd, tempName, unix.AT_FDCWD, dest)
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("moving out of the sync folder: %w", err)
	}

	if err := copyThroughDescriptor(file, dest); err != nil {
		return err
	}
	return withFd(parent, func(fd int) error {
		if unlinkErr := unix.Unlinkat(fd, tempName, 0); unlinkErr != nil {
			return fmt.Errorf("removing the set-aside file after copying it out: %w", unlinkErr)
		}
		return nil
	})
}

// copyThroughDescriptor writes the content of the verified descriptor to
// dest atomically: a temporary sibling, fsync, rename.
func copyThroughDescriptor(file *os.File, dest string) (err error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding file to copy it: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), detachDirectoryPerm); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".copy-*")
	if err != nil {
		return fmt.Errorf("creating copy of file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup of a failed copy
		}
	}()
	if _, err = io.Copy(tmp, file); err != nil {
		_ = tmp.Close() //nolint:errcheck // the copy error is reported
		return fmt.Errorf("copying file: %w", err)
	}
	if err = tmp.Chmod(detachCopyFilePerm); err != nil {
		_ = tmp.Close() //nolint:errcheck // the chmod error is reported
		return fmt.Errorf("setting copy permissions: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close() //nolint:errcheck // the sync error is reported
		return fmt.Errorf("syncing copy: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing copy: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("placing copy: %w", err)
	}
	return nil
}
