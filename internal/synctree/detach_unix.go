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

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ErrDetachRaced reports that the entry at a path stopped being the file that
// was verified before it could be taken out of the tree. Nothing was removed;
// whatever is at the path now is left there.
var ErrDetachRaced = errors.New("the file changed while it was being removed")

// ErrDestinationFailed reports that the place outside the tree a file was
// being taken or copied out to failed: its folder could not be opened, was
// gone or swapped by the time the file moved, or the move or copy into it
// failed or found the name taken. The file stays in the tree. The cause is
// named as text, not wrapped, so a destination whose folder is gone never
// reads as a file that is gone (os.ErrNotExist), and a destination that is
// full or refused never reads as the state of the sync folder.
var ErrDestinationFailed = errors.New("the destination outside the sync folder failed")

// detachTempPrefix and detachTempSuffix name a file set aside while it is
// being removed. Local observation ignores the name (IsDetachArtifactName),
// and unlike a .partial download it is never cleaned up automatically: if a
// run dies between setting a file aside and moving it out, the file left
// behind is user content, which a full scan reports instead of deleting.
// detachCopyPrefix names the copy a cross-filesystem take-out writes beside
// its destination before giving it the destination's name.
const (
	detachTempPrefix   = ".onedrive-go."
	detachTempSuffix   = ".detach"
	detachCopyPrefix   = ".copy-"
	detachRandomBytes  = 8
	detachCopyFilePerm = 0o600
)

// destinationFolderFlags open the folder a file is taken or copied out to as
// parentFolderFlags open a parent, and never through a symbolic link in its
// last name: the folder the checks see is the one every later step is made
// on.
const destinationFolderFlags = parentFolderFlags | unix.O_NOFOLLOW

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
// dest's folder is opened once, before anything else (openDestination), and
// the move is made on that descriptor and dest's base name, never by path, so
// the folder swapped for a symbolic link or removed meanwhile fails the move
// instead of sending the file where the link points. A dest inside the tree,
// or the tree's folder itself, is refused (ErrUnsafePath) before anything
// moves; a dest that fails, its folder missing, a link, or gone by the time
// the file moves, the move or copy into it refused, or its name taken, fails
// with ErrDestinationFailed and the file stays in the tree. Nothing at dest's
// name is replaced.
//
// Hashing by path and then removing by path would unlink whatever is at the
// path by then: an editor's atomic save during a long hash would lose the new
// content (D-010). So the file is opened once without following links,
// verified through that descriptor, renamed to a temporary sibling inside the
// tree, and the sibling's identity is compared with the descriptor's. Only a
// match leaves the tree; a mismatch is put back and reported as
// ErrDetachRaced. dest lands by rename on the same filesystem and by copy
// through the verified descriptor across filesystems.
func (r *Root) DetachRegularFile(rel string, verify VerifyFunc, dest string) error {
	if r.memory != nil {
		return destinationFailed(fmt.Errorf("moving %s to %s: %w", rel, dest, ErrOutsideMemoryVolume))
	}
	out, err := r.openDestination(dest)
	if err != nil {
		return err
	}
	defer out.close()
	v, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer v.close()
	parent, name, file, info, stamp := v.parent, v.name, v.file, v.info, v.stamp

	tempName, err := r.setAsideVerified(parent, name, file, info, stamp, rel)
	if err != nil {
		return err
	}

	if err := r.moveDetachedOut(parent, tempName, file, stamp, out); err != nil {
		if restoreErr := r.linkBackNoReplace(parent, tempName, name); restoreErr != nil {
			return fmt.Errorf("%w; putting %s back: %w", err, rel, restoreErr)
		}
		return err
	}

	return nil
}

// RemoveVerifiedRegularFile permanently removes the regular file at rel, but
// only the inode verify approved; see DetachRegularFile for the binding.
func (r *Root) RemoveVerifiedRegularFile(rel string, verify VerifyFunc) error {
	if r.memory != nil {
		clean, err := cleanRelative(rel)
		if err != nil {
			return err
		}
		if clean == "." {
			return unsafePathf("refusing to remove the sync folder itself")
		}
		return r.memory.removeVerified(clean, r.nameForMessage(clean), verify)
	}
	v, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer v.close()
	parent, name, file, info, stamp := v.parent, v.name, v.file, v.info, v.stamp

	tempName, err := r.setAsideVerified(parent, name, file, info, stamp, rel)
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

// PreserveRegularFile copies the verified regular file at rel to dest, a path
// outside the tree, without taking it out of the tree: a hard link where one
// can be made, a copy through the verified descriptor across filesystems or
// where the filesystem cannot make the link (localpath.IsLinkUnsupported). A
// download that then replaces rel by rename leaves the preserved inode
// untouched. dest is reached and refused as DetachRegularFile reaches and
// refuses it, through its folder's one descriptor: a dest inside the tree is
// ErrUnsafePath, and a dest whose folder cannot be opened or is gone by the
// time of the link, whose name is taken, or a copy into it that fails, is
// ErrDestinationFailed.
func (r *Root) PreserveRegularFile(rel string, verify VerifyFunc, dest string) error {
	if r.memory != nil {
		return destinationFailed(fmt.Errorf("preserving %s at %s: %w", rel, dest, ErrOutsideMemoryVolume))
	}
	out, err := r.openDestination(dest)
	if err != nil {
		return err
	}
	defer out.close()
	v, err := r.openVerifiedRegular(rel, verify)
	if err != nil {
		return err
	}
	defer v.close()
	parent, name, file, info, stamp := v.parent, v.name, v.file, v.info, v.stamp

	if err := unchangedBeforeMove(file, stamp); err != nil {
		return fmt.Errorf("preserving %s: %w", rel, err)
	}
	linkErr := r.ops.link(parent, name, out.folder, out.name)
	switch {
	case linkErr == nil:
		return r.confirmPreservedLink(out, file, info, stamp, rel)
	case errors.Is(linkErr, unix.EXDEV), localpath.IsLinkUnsupported(linkErr):
		return r.copyVerified(file, stamp, out)
	case errors.Is(linkErr, unix.EEXIST),
		errors.Is(linkErr, unix.ENOENT) && sameFileAt(parent, name, info) == nil:
		// The name is taken at the destination, or the file is still at its
		// own name, so the folder the link was to land in is gone.
		return destinationFailed(fmt.Errorf("preserving %s at %s: %w", rel, out.path, linkErr))
	default:
		// The link's own errno does not tell which side refused it.
		return fmt.Errorf("preserving %s: %w", rel, linkErr)
	}
}

// confirmPreservedLink proves the link PreserveRegularFile made at out names
// the verified inode, still holding the verified content. The link is by
// name, so a link whose content changed meanwhile is removed, while a name
// that no longer holds the verified inode, or cannot be read, is left as it
// is: it is not the link this call made, and removing it could destroy what
// another program put there. Either way the preserve is ErrDetachRaced.
func (r *Root) confirmPreservedLink(out *destination, file *os.File, info os.FileInfo, stamp fileStamp, rel string) error {
	switch err := sameFileAt(out.folder, out.name, info); {
	case errors.Is(err, ErrDetachRaced):
		return fmt.Errorf("preserving %s: %w", rel, err)
	case err != nil:
		return fmt.Errorf("preserving %s: %w (%s)", rel, ErrDetachRaced, err.Error())
	}
	if unchangedSince(file, stamp) == nil {
		return nil
	}
	if err := r.ops.unlink(out.folder, out.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return destinationFailed(fmt.Errorf("discarding stale copy of %s: %w", rel, err))
	}

	return fmt.Errorf("preserving %s: %w", rel, ErrDetachRaced)
}

// destinationFailed is err, a failure of the destination side of a take-out
// or copy-out, as ErrDestinationFailed with its cause named as text.
func destinationFailed(err error) error {
	return fmt.Errorf("%w: %s", ErrDestinationFailed, err.Error())
}

// verifiedFile is a regular file opened and approved by a VerifyFunc, with
// the directory it was opened through.
type verifiedFile struct {
	parent *os.File
	name   string
	file   *os.File
	info   os.FileInfo
	stamp  fileStamp
}

func (v *verifiedFile) close() {
	_ = v.file.Close()   //nolint:errcheck // read-only file descriptor
	_ = v.parent.Close() //nolint:errcheck // read-only directory descriptor
}

// destination is the place outside the tree a file is taken or copied out
// to: its folder, opened once (openDestination), and the name the file takes
// in it. Every step of a take-out or copy-out there, the rename, the link,
// the copy's create and rename and the removal of a stale or torn copy, is
// made on folder and name, never on a path, so a folder swapped for a
// symbolic link after it was opened is never followed: a step made in a
// folder removed meanwhile fails.
type destination struct {
	folder *os.File
	name   string
	// path is dest as the caller gave it, for messages.
	path string
}

func (d *destination) close() {
	closeDirectory(d.folder)
}

// openDestination opens the folder of dest, an absolute path outside the
// tree, once, without following a symbolic link in its last name, and
// refuses it when it is the root or lies inside it (refuseDestinationInside).
// Links above that folder are followed as the path resolves, and the folder
// they lead to is the one checked, then used. A folder that cannot be opened,
// missing, not a folder or a link, is ErrDestinationFailed.
func (r *Root) openDestination(dest string) (*destination, error) {
	clean := filepath.Clean(dest)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) == clean {
		return nil, unsafePathf("%q is not the absolute path of a file outside the sync folder", dest)
	}
	folder, err := localpath.OpenFile(filepath.Dir(clean), destinationFolderFlags, 0)
	if err != nil {
		return nil, destinationFailed(fmt.Errorf("opening the folder of %s: %w", dest, err))
	}
	out := &destination{folder: folder, name: filepath.Base(clean), path: dest}
	if err := r.refuseDestinationInside(out); err != nil {
		out.close()
		return nil, err
	}

	return out, nil
}

// refuseDestinationInside refuses out, where a file taken or copied out of
// the tree goes, when it is the root or lies inside it: the move would rename
// the file within the tree, or link it beside itself, instead of taking it
// out. The answer comes from what the folders are, not from a path's text
// (refuseFolderWithinRoot): out's name when it opens on out's folder as a
// folder, which is how the root itself is refused, and out's folder. So a
// dest whose text lies in the tree but that a link above its folder places
// outside it is taken, and one that a link outside places inside it is
// refused.
func (r *Root) refuseDestinationInside(out *destination) error {
	const refusal = "not a place to take a file out to"
	if named, err := openatFolderNoFollow(out.folder, out.name); err == nil {
		refuseErr := r.refuseFolderWithinRoot(named, out.path, refusal)
		closeDirectory(named)
		if refuseErr != nil {
			return refuseErr
		}
	}

	return r.refuseFolderWithinRoot(out.folder, out.path, refusal)
}

// openVerifiedRegular opens the regular file at rel through its parent
// folder, opened through the root as a folder that never blocks on a FIFO
// swapped in for it (openParentFolder), and runs verify on it.
func (r *Root) openVerifiedRegular(rel string, verify VerifyFunc) (*verifiedFile, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return nil, unsafePathf("refusing to remove the sync folder itself")
	}
	if ancestorErr := r.ValidateNoSymlinkAncestors(clean); ancestorErr != nil {
		return nil, ancestorErr
	}

	parent, err := r.openParentFolder(clean)
	if err != nil {
		return nil, err
	}
	v := &verifiedFile{parent: parent, name: filepath.Base(clean)}
	v.file, err = openatNoFollow(parent, v.name)
	if err != nil {
		_ = parent.Close() //nolint:errcheck // error path; the open failure is reported
		return nil, &os.PathError{Op: "open", Path: r.nameForMessage(clean), Err: err}
	}
	if err := v.check(r.nameForMessage(clean), verify); err != nil {
		v.close()
		return nil, err
	}

	return v, nil
}

// check stamps the open file and runs verify on it. The stamp is taken before
// the check, so a write during or after it is caught when the file is moved.
func (v *verifiedFile) check(displayName string, verify VerifyFunc) error {
	info, err := v.file.Stat()
	if err != nil {
		return fmt.Errorf("stating %s: %w", displayName, err)
	}
	if !info.Mode().IsRegular() {
		return unsupportedEntryf("%s is not a regular file", displayName)
	}
	v.info = info
	if v.stamp, err = stampOf(v.file); err != nil {
		return err
	}
	if verify != nil {
		return verify(v.file, info)
	}
	return nil
}

// setAsideVerified renames name to a temporary sibling and proves the
// sibling is the verified inode, unchanged since it was verified. On a
// mismatch the sibling is put back and ErrDetachRaced returned.
func (r *Root) setAsideVerified(
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
	if err := r.ops.renameAt(parent, name, parent, tempName); err != nil {
		return "", fmt.Errorf("setting aside %s: %w", rel, err)
	}
	checkErr := sameFileAt(parent, tempName, info)
	if checkErr == nil {
		checkErr = unchangedSince(file, stamp)
	}
	if checkErr != nil {
		if restoreErr := r.linkBackNoReplace(parent, tempName, name); restoreErr != nil {
			return "", fmt.Errorf("%w; putting %s back: %w", checkErr, rel, restoreErr)
		}
		return "", fmt.Errorf("removing %s: %w", rel, checkErr)
	}

	return tempName, nil
}

func detachTempName() (string, error) {
	return randomName(detachTempPrefix, detachTempSuffix)
}

// randomName is prefix, detachRandomBytes random bytes in hex, and suffix: a
// name no other entry of a folder holds unless a run that died left it.
func randomName(prefix, suffix string) (string, error) {
	var random [detachRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("naming a temporary file: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]) + suffix, nil
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
	return openatFlags(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK)
}

// openatFolderNoFollow opens name in parent only as a folder, never through a
// symbolic link and never blocking on a FIFO.
func openatFolderNoFollow(parent *os.File, name string) (*os.File, error) {
	return openatFlags(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK)
}

func openatFlags(parent *os.File, name string, flags int) (*os.File, error) {
	var fd int
	err := withFd(parent, func(dirfd int) error {
		var openErr error
		fd, openErr = unix.Openat(dirfd, name, flags, 0)
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
		return fmt.Errorf("checking %s: %w", name, err)
	}
	wantID, ok := IdentityFromFileInfo(want)
	if !ok || statFieldToUint64(st.Dev) != wantID.Device || statFieldToUint64(st.Ino) != wantID.Inode {
		return ErrDetachRaced
	}
	return nil
}

// linkBackNoReplace returns a set-aside file to its name without replacing
// anything that appeared there meanwhile: link fails with EEXIST instead of
// overwriting. On a filesystem that cannot make the link
// (localpath.IsLinkUnsupported) the file is renamed back instead, with the
// system's no-replace flag or, where that is refused too, the check that the
// name is free and the plain rename the no-replace rename falls back to
// (renameExclusiveOrChecked). If the name is taken, the set-aside file stays
// under its temporary name rather than destroying either version.
func (r *Root) linkBackNoReplace(dir *os.File, tempName string, name string) error {
	linkErr := r.ops.link(dir, tempName, dir, name)
	switch {
	case linkErr == nil:
		if err := r.ops.unlink(dir, tempName); err != nil {
			return fmt.Errorf("removing %s after restoring it as %s: %w", tempName, name, err)
		}
		return nil
	case localpath.IsLinkUnsupported(linkErr):
		if err := r.renameExclusiveOrChecked(dir, tempName, dir, name); err != nil {
			return fmt.Errorf("restoring %s (kept as %s): %w", name, tempName, err)
		}
		return nil
	default:
		return fmt.Errorf("restoring %s (kept as %s): %w", name, tempName, linkErr)
	}
}

// renameExclusiveOrChecked renames from in fromDir to to in toDir without
// replacing anything at to: the system's no-replace flag or, where it is
// refused, a lstat of to that finds nothing and then a plain rename
// (renameCheckedAt), which replaces only an entry made at to between the two.
// It takes no hard link, so a file it moves is never left under two names.
func (r *Root) renameExclusiveOrChecked(fromDir *os.File, from string, toDir *os.File, to string) error {
	refused, err := r.ops.renameExclusive(fromDir, from, toDir, to)
	if !refused {
		return err
	}

	return r.renameCheckedAt(fromDir, from, toDir, to)
}

// moveDetachedOut moves the set-aside file to out, copying when out is on
// another filesystem. The move never replaces an entry at out's name
// (renameExclusiveOrChecked). The source is unlinked only once the copy is
// durable (its bytes and the directory entry that names them): unlinking
// before that left, after a power cut, a trash entry with only a `.copy-*`
// file and no original anywhere. A rename that fails is the destination's
// failure (ErrDestinationFailed): the set-aside sibling was just found in
// place, in a folder the set-aside rename could write.
func (r *Root) moveDetachedOut(parent *os.File, tempName string, file *os.File, stamp fileStamp, out *destination) error {
	err := r.renameExclusiveOrChecked(parent, tempName, out.folder, out.name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EXDEV) {
		return destinationFailed(fmt.Errorf("moving out of the sync folder to %s: %w", out.path, err))
	}

	if err := r.copyVerified(file, stamp, out); err != nil {
		return err
	}
	if unlinkErr := r.ops.unlink(parent, tempName); unlinkErr != nil {
		return fmt.Errorf("removing the set-aside file after copying it out: %w", unlinkErr)
	}
	return nil
}

// copyVerified copies through the verified descriptor and then re-checks it:
// a write that landed while the copy ran means the copy may be torn, so it is
// discarded and the move reported as raced. A copy that fails is the
// destination's failure (ErrDestinationFailed); a discard that fails is named
// as text, so the result matches the race only.
func (r *Root) copyVerified(file *os.File, stamp fileStamp, out *destination) error {
	copyID, err := r.copyThroughDescriptor(file, out)
	if err != nil {
		return destinationFailed(err)
	}
	if err := unchangedSince(file, stamp); err != nil {
		if removeErr := r.discardOwnCopy(out, copyID); removeErr != nil {
			return fmt.Errorf("%w; discarding the copy: %s", err, removeErr.Error())
		}
		return err
	}
	return nil
}

// discardOwnCopy removes the copy this detach placed at out's name, and only
// while the name still holds it: something else put at the name since (a
// same-user rename into the trash entry) is left where it is and reported.
// A name already gone is nothing to discard.
func (r *Root) discardOwnCopy(out *destination, copyID FileIdentity) error {
	st, err := r.ops.lstatEntry(out.folder, out.name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("checking the copy at %s before discarding it: %w", out.path, err)
	}
	held := FileIdentity{Device: statFieldToUint64(st.Dev), Inode: statFieldToUint64(st.Ino)}
	if !SameIdentity(held, copyID) {
		return fmt.Errorf("%s no longer holds the copy, so it is left as it is", out.path)
	}
	if err := r.ops.unlink(out.folder, out.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the copy at %s: %w", out.path, err)
	}

	return nil
}

// copyThroughDescriptor writes the content of the verified descriptor to out
// atomically and durably, all on out's folder descriptor: a temporary sibling
// created exclusively, fsync, a rename that never replaces an entry at out's
// name (renameExclusiveOrChecked), and an fsync of the folder, because the
// rename is a directory update that is not durable until the directory is. A
// directory sync the filesystem does not support (FUSE, SMB) is tolerated;
// any other failure removes the copy and is reported, so the caller never
// unlinks the source over a copy that may not survive a crash. It returns the
// copy's identity, so a later discard removes only the copy.
func (r *Root) copyThroughDescriptor(file *os.File, out *destination) (copyID FileIdentity, err error) {
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return FileIdentity{}, fmt.Errorf("rewinding file to copy it: %w", err)
	}
	tmpName, err := randomName(detachCopyPrefix, "")
	if err != nil {
		return FileIdentity{}, err
	}
	tmp, err := openatExclusive(out.folder, tmpName, detachCopyFilePerm)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("creating a copy beside %s: %w", out.path, err)
	}
	defer func() {
		if err != nil {
			err = discardPartialCopy(tmp, out.folder, tmpName, err, r.ops.unlink)
		}
	}()
	if _, err = io.Copy(tmp, file); err != nil {
		return FileIdentity{}, fmt.Errorf("copying file: %w", err)
	}
	if err = tmp.Chmod(detachCopyFilePerm); err != nil {
		return FileIdentity{}, fmt.Errorf("setting copy permissions: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return FileIdentity{}, fmt.Errorf("syncing copy: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return FileIdentity{}, fmt.Errorf("reading the copy's identity: %w", err)
	}
	copyID, ok := IdentityFromFileInfo(info)
	if !ok {
		return FileIdentity{}, errors.New("reading the copy's identity: no device and inode")
	}
	if err = tmp.Close(); err != nil {
		return FileIdentity{}, fmt.Errorf("closing copy: %w", err)
	}
	if err = r.renameExclusiveOrChecked(out.folder, tmpName, out.folder, out.name); err != nil {
		return FileIdentity{}, fmt.Errorf("placing the copy at %s: %w", out.path, err)
	}
	if err = r.ops.syncDirFile(out.folder); err != nil && !localpath.IsDirSyncUnsupported(err) {
		syncErr := err
		if removeErr := r.discardOwnCopy(out, copyID); removeErr != nil {
			return FileIdentity{}, fmt.Errorf("making the copy durable: %w; discarding it: %w", syncErr, removeErr)
		}
		return FileIdentity{}, fmt.Errorf("making the copy durable: %w", syncErr)
	}
	return copyID, nil
}
