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
	"strings"

	"golang.org/x/sys/unix"
)

// ErrDetachRaced reports that the entry at a path stopped being the file that
// was verified before it could be taken out of the tree. Nothing was removed;
// whatever is at the path now is left there.
var ErrDetachRaced = errors.New("the file changed while it was being removed")

// detachTempPrefix and detachTempSuffix name a file set aside while it is
// being removed. Local observation ignores the name (IsDetachArtifactName),
// and unlike a .partial download it is never cleaned up automatically: if a
// run dies between setting a file aside and moving it out, the file left
// behind is user content, which a full scan reports instead of deleting.
const (
	detachTempPrefix    = ".onedrive-go."
	detachTempSuffix    = ".detach"
	detachRandomBytes   = 8
	detachCopyFilePerm  = 0o600
	detachDirectoryPerm = 0o700
)

// IsDetachArtifactName reports whether name is a file this package set aside
// while removing it.
func IsDetachArtifactName(name string) bool {
	return strings.HasPrefix(name, detachTempPrefix) && strings.HasSuffix(name, detachTempSuffix)
}

// fileStamp is what a verified file must still be when it leaves the tree:
// an in-place write after the content check changes at least one of these,
// even when it keeps the inode.
type fileStamp struct {
	size  int64
	mtime unix.Timespec
	ctime unix.Timespec
}

func stampOf(file *os.File) (fileStamp, error) {
	var st unix.Stat_t
	if err := withFd(file, func(fd int) error { return unix.Fstat(fd, &st) }); err != nil {
		return fileStamp{}, fmt.Errorf("stating verified file: %w", err)
	}
	return fileStamp{size: st.Size, mtime: st.Mtim, ctime: st.Ctim}, nil
}

// unchangedSince fails with ErrDetachRaced when the open file is no longer
// what was verified: written in place between the check and the move.
// unchangedBeforeMove also compares ctime; after the set-aside rename only
// size and mtime are compared, because renaming the file updates its ctime.
func unchangedSince(file *os.File, verified fileStamp) error {
	now, err := stampOf(file)
	if err != nil {
		return err
	}
	if now.size != verified.size || now.mtime != verified.mtime {
		return ErrDetachRaced
	}
	return nil
}

func unchangedBeforeMove(file *os.File, verified fileStamp) error {
	now, err := stampOf(file)
	if err != nil {
		return err
	}
	if now != verified {
		return ErrDetachRaced
	}
	return nil
}

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
	parent, name, file, info, stamp, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	tempName, err := setAsideVerified(parent, name, file, info, stamp, rel)
	if err != nil {
		return err
	}

	if err := moveDetachedOut(parent, tempName, file, stamp, dest); err != nil {
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
	parent, name, file, info, stamp, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	tempName, err := setAsideVerified(parent, name, file, info, stamp, rel)
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
	parent, name, file, info, stamp, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // read-only directory descriptor
	defer file.Close()   //nolint:errcheck // read-only file descriptor

	if err := unchangedBeforeMove(file, stamp); err != nil {
		return fmt.Errorf("preserving %s: %w", rel, err)
	}
	linkErr := linkatOut(parent, name, dest)
	switch {
	case linkErr == nil:
		// The link is by name; prove it names the verified inode, still
		// holding the verified content.
		linked, statErr := os.Lstat(dest)
		if statErr == nil && os.SameFile(linked, info) && unchangedSince(file, stamp) == nil {
			return nil
		}
		if removeErr := os.Remove(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("discarding stale copy of %s: %w", rel, removeErr)
		}
		return fmt.Errorf("preserving %s: %w", rel, ErrDetachRaced)
	case errors.Is(linkErr, unix.EXDEV), errors.Is(linkErr, unix.EPERM), errors.Is(linkErr, unix.EMLINK):
		return copyVerified(file, stamp, dest)
	default:
		return fmt.Errorf("preserving %s: %w", rel, linkErr)
	}
}

func (r *Root) openVerifiedRegular(
	rel string,
	verify VerifyFunc,
) (parent *os.File, name string, file *os.File, info os.FileInfo, stamp fileStamp, err error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, "", nil, nil, fileStamp{}, err
	}
	if clean == "." {
		return nil, "", nil, nil, fileStamp{}, unsafePathf("refusing to remove the sync folder itself")
	}
	if ancestorErr := r.ValidateNoSymlinkAncestors(clean); ancestorErr != nil {
		return nil, "", nil, nil, fileStamp{}, ancestorErr
	}

	parentRel := filepath.Dir(clean)
	name = filepath.Base(clean)
	parent, err = r.Open(parentRel)
	if err != nil {
		return nil, "", nil, nil, fileStamp{}, err
	}

	file, err = openatNoFollow(parent, name)
	if err != nil {
		_ = parent.Close() //nolint:errcheck // error path; the open failure is reported
		return nil, "", nil, nil, fileStamp{}, &os.PathError{Op: "open", Path: r.nameForMessage(clean), Err: err}
	}
	info, err = file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = unsupportedEntryf("%s is not a regular file", r.nameForMessage(clean))
	}
	if err == nil {
		// The stamp is taken before the check, so a write during or after
		// it is caught when the file is moved.
		stamp, err = stampOf(file)
	}
	if err == nil && verify != nil {
		err = verify(file, info)
	}
	if err != nil {
		_ = file.Close()   //nolint:errcheck // error path; the verify failure is reported
		_ = parent.Close() //nolint:errcheck // error path; the verify failure is reported
		return nil, "", nil, nil, fileStamp{}, err
	}

	return parent, name, file, info, stamp, nil
}

// setAsideVerified renames name to a temporary sibling and proves the
// sibling is the verified inode, unchanged since it was verified. On a
// mismatch the sibling is put back and ErrDetachRaced returned.
func setAsideVerified(
	parent *os.File,
	name string,
	file *os.File,
	info os.FileInfo,
	stamp fileStamp,
	rel string,
) (string, error) {
	tempName, err := detachTempName()
	if err != nil {
		return "", err
	}
	if err := unchangedBeforeMove(file, stamp); err != nil {
		return "", fmt.Errorf("removing %s: %w", rel, err)
	}
	if err := renameat(parent, name, parent, tempName); err != nil {
		return "", fmt.Errorf("setting aside %s: %w", rel, err)
	}
	checkErr := sameFileAt(parent, tempName, info)
	if checkErr == nil {
		checkErr = unchangedSince(file, stamp)
	}
	if checkErr != nil {
		if restoreErr := linkBackNoReplace(parent, tempName, name); restoreErr != nil {
			return "", fmt.Errorf("%w; putting %s back: %w", checkErr, rel, restoreErr)
		}
		return "", fmt.Errorf("removing %s: %w", rel, checkErr)
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
		opErr = fn(int(fd))
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
		if openErr != nil {
			return fmt.Errorf("opening without following links: %w", openErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
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

func moveDetachedOut(parent *os.File, tempName string, file *os.File, stamp fileStamp, dest string) error {
	err := withFd(parent, func(fd int) error {
		return unix.Renameat(fd, tempName, unix.AT_FDCWD, dest)
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("moving out of the sync folder: %w", err)
	}

	if err := copyVerified(file, stamp, dest); err != nil {
		return err
	}
	return withFd(parent, func(fd int) error {
		if unlinkErr := unix.Unlinkat(fd, tempName, 0); unlinkErr != nil {
			return fmt.Errorf("removing the set-aside file after copying it out: %w", unlinkErr)
		}
		return nil
	})
}

// copyVerified copies through the verified descriptor and then re-checks it:
// a write that landed while the copy ran means the copy may be torn, so it is
// discarded and the move reported as raced.
func copyVerified(file *os.File, stamp fileStamp, dest string) error {
	if err := copyThroughDescriptor(file, dest); err != nil {
		return err
	}
	if err := unchangedSince(file, stamp); err != nil {
		if removeErr := os.Remove(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("%w; discarding the copy: %w", err, removeErr)
		}
		return err
	}
	return nil
}

// copyThroughDescriptor writes the content of the verified descriptor to
// dest atomically: a temporary sibling, fsync, rename.
func copyThroughDescriptor(file *os.File, dest string) (err error) {
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding file to copy it: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(dest), detachDirectoryPerm); err != nil {
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
