//go:build unix

package synctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ErrTargetExists reports that a no-replace rename found something at its
// destination, an empty folder included, and left both names as they were.
var ErrTargetExists = errors.New("destination already exists")

// ErrSourceNotRemoved reports that a no-replace rename put the entry at its
// destination but could not remove the source, which happens only where the
// system refuses the no-replace flag and the entry is linked, or copied into
// the root, and its source then cannot be removed, or cannot be checked to
// still hold the entry before it is. The move itself succeeded; callers
// treat it as success and report the leftover.
var ErrSourceNotRemoved = errors.New("the file is in place but the original could not be removed")

// RenameNoReplace moves srcRel to dstRel inside the sync root, the entry
// itself and never a copy of it, and never replaces anything at dstRel: an
// entry there, an empty folder included, fails the move with ErrTargetExists
// and leaves both names as they were. A plain rename replaces a file or an
// empty folder at its destination silently.
//
// Both parent folders are opened through the root, as folders, and the move
// is made on those two descriptors and the two base names, so a folder on
// either path swapped for a symlink out of the root, or for anything that is
// not a folder, at any moment fails the open (ErrUnsafePath) instead of
// carrying the move outside. The final names are not followed. The move is
// atomic where the system offers the flag (renameat2 RENAME_NOREPLACE on
// Linux, renameatx_np RENAME_EXCL on macOS). Where the kernel or the
// filesystem refuses it, the entry is hard-linked at dstRel and unlinked from
// srcRel, a link failing on an existing name as the flag does, and srcRel is
// left alone when it no longer names the linked entry, or when that cannot be
// checked (ErrSourceNotRemoved). Where the
// entry cannot be linked either (a folder, a filesystem without hard links,
// too many links), a lstat of dstRel on its folder's descriptor that finds
// anything fails the move with ErrTargetExists, and otherwise a plain rename
// on the two descriptors moves it: the one place a no-replace rename checks
// and then renames, so an entry made at dstRel between the two is replaced if
// a plain rename would replace it. A copy would not have that window, but it
// gives the entry a new identity and loses what a program writes to it while
// it is copied. The rename's own failure takes the rule of every rooted
// operation (normalizeNotExist) on srcRel.
func (r *Root) RenameNoReplace(srcRel, dstRel string) error {
	srcClean, err := cleanEntryName(srcRel)
	if err != nil {
		return err
	}
	dstClean, err := cleanEntryName(dstRel)
	if err != nil {
		return err
	}
	if r.memory != nil {
		err = r.memory.rename(srcClean, dstClean, true)
		if err != nil && !errors.Is(err, os.ErrExist) {
			err = r.normalizeDescriptorFailure(srcClean, err)
		}
		return noReplaceResult("renaming", srcClean, dstClean, err)
	}

	srcParent, err := r.openParentFolder(srcClean)
	if err != nil {
		return fmt.Errorf("renaming %s to %s: %w", srcClean, dstClean, err)
	}
	defer closeDirectory(srcParent)
	dstParent, err := r.openParentFolder(dstClean)
	if err != nil {
		return fmt.Errorf("renaming %s to %s: %w", srcClean, dstClean, err)
	}
	defer closeDirectory(dstParent)

	err = r.renameNoReplaceAt(srcParent, filepath.Base(srcClean), dstParent, filepath.Base(dstClean))
	if err != nil && !errors.Is(err, os.ErrExist) && !errors.Is(err, ErrSourceNotRemoved) {
		err = r.normalizeDescriptorFailure(srcClean, err)
	}

	return noReplaceResult("renaming", srcClean, dstClean, err)
}

// MoveInNoReplace moves srcAbs, a file outside the sync root, to dstRel
// inside it, never replacing anything at dstRel, as RenameNoReplace does.
// Only the destination is resolved through the root: srcAbs lies in a folder
// the caller manages outside every sync tree (the local trash), so its parent
// is opened by path, also as a folder. A parent that is the sync root or lies
// below it is refused (ErrUnsafePath), decided by what the opened folder is,
// not by the path's text (refuseFolderWithinRoot), so a trash path that
// reaches the sync folder through a symbolic link, or through another
// spelling on a volume that folds case, is refused too. Where the flag is
// refused the file is linked and unlinked, and
// where it cannot be linked a regular file is created at dstRel exclusively,
// copied, synced, its folder synced and only then the source removed, while
// its name still holds the file copied, which
// is not atomic (a crash can leave both) but never replaces anything; an entry
// that is not a regular file fails with the rename's own refusal, which also
// matches localpath.ErrNotRegularFile. A move across filesystems fails with
// an error matching syscall.EXDEV and leaves both names as they were; the
// caller copies instead.
func (r *Root) MoveInNoReplace(srcAbs, dstRel string) error {
	if !filepath.IsAbs(srcAbs) {
		return unsafePathf("%q is not an absolute path outside the sync folder", srcAbs)
	}
	dstClean, err := cleanEntryName(dstRel)
	if err != nil {
		return err
	}
	if r.memory != nil {
		return noReplaceResult("moving", srcAbs, dstClean, ErrOutsideMemoryVolume)
	}

	srcParent, err := localpath.OpenFile(filepath.Dir(srcAbs), parentFolderFlags, 0)
	if err != nil {
		return fmt.Errorf("moving %s to %s: %w", srcAbs, dstClean, err)
	}
	defer closeDirectory(srcParent)
	if refuseErr := r.refuseFolderWithinRoot(srcParent, srcAbs, "not a file to move into it"); refuseErr != nil {
		return refuseErr
	}
	dstParent, err := r.openParentFolder(dstClean)
	if err != nil {
		return fmt.Errorf("moving %s to %s: %w", srcAbs, dstClean, err)
	}
	defer closeDirectory(dstParent)

	err = r.moveInNoReplaceAt(srcParent, filepath.Base(srcAbs), dstParent, filepath.Base(dstClean))

	return noReplaceResult("moving", srcAbs, dstClean, err)
}

// WithoutNoReplaceFlagOrHardLinks returns a Root over the same folder whose
// no-replace renames find neither the system's flag nor hard links, as on a
// filesystem that offers neither, so they take their last step: the check
// and plain rename inside the root, the exclusive copy into it. It is
// exported for tests in other packages, as Interpose is.
func (r *Root) WithoutNoReplaceFlagOrHardLinks() *Root {
	ops := r.ops
	ops.renameExclusive = refuseNoReplaceFlag
	ops.link = func(*os.File, string, *os.File, string) error { return unix.EPERM }

	return &Root{dir: r.dir, ops: ops, memory: r.memory}
}

// WithoutNoReplaceFlagAndUnremovableName returns a Root over the same folder
// whose no-replace renames find no system flag but find hard links, so they
// link the entry and unlink its source, and whose unlink of an entry called
// name, in any folder, fails with EBUSY: a rename from that name places the
// entry and reports the source it could not remove (ErrSourceNotRemoved). It
// is exported for tests in other packages, as Interpose is.
func (r *Root) WithoutNoReplaceFlagAndUnremovableName(name string) *Root {
	ops := r.ops
	ops.renameExclusive = refuseNoReplaceFlag
	unlink := ops.unlink
	ops.unlink = func(dir *os.File, base string) error {
		if base == name {
			return unix.EBUSY
		}

		return unlink(dir, base)
	}

	return &Root{dir: r.dir, ops: ops, memory: r.memory}
}

// refuseNoReplaceFlag is renameExclusive on a system that refuses the flag.
func refuseNoReplaceFlag(*os.File, string, *os.File, string) (bool, error) {
	return true, unix.EINVAL
}

// parentFolderFlags open a parent folder only as a folder: O_DIRECTORY fails
// on anything else. On Linux it already fails a FIFO swapped in for the
// folder before the open can wait for a writer; O_NONBLOCK is there for a
// system where O_DIRECTORY would not, so the open never blocks.
const parentFolderFlags = os.O_RDONLY | unix.O_DIRECTORY | unix.O_NONBLOCK

// openParentFolder opens the folder that holds clean through the root. When
// that folder or one above it is a symbolic link or not a folder, the failure
// is the ancestor walk's refusal (ErrUnsafePath), which names it; the open's
// own error says only that the path escapes or is not a directory.
func (r *Root) openParentFolder(clean string) (*os.File, error) {
	dir, err := r.OpenFile(filepath.Dir(clean), parentFolderFlags, 0)
	if err == nil {
		return dir, nil
	}
	if ancestorErr := r.ValidateNoSymlinkAncestors(clean); errors.Is(ancestorErr, ErrUnsafePath) {
		return nil, ancestorErr
	}

	return nil, err
}

// maxFoldersAboveAFolder bounds the walk refuseFolderWithinRoot makes from a
// folder up to the top of its filesystem, so the walk ends with an error
// instead of running on where no folder above reports itself as its own
// parent. A real folder tree reaches its top in far fewer steps.
const maxFoldersAboveAFolder = 1024

// refuseFolderWithinRoot refuses dir, an open folder outside the root's
// handle, when it is the sync folder or lies below it: name, the path in it
// the caller would move a file from or to, is then in the tree, and the
// refusal says so, followed by refusal. The answer comes from what the
// folders are, not from a path's text: dir and each folder above it,
// reached by "..", "../.." and so on from dir's descriptor without following
// any link, are compared by device and inode with the sync folder as its root
// handle sees it, until the walk reaches the folder that is its own parent.
// So a path to dir that went through a symbolic link into the tree, or that
// spelled a folder of the tree another way on a volume that folds case, is
// still found inside.
func (r *Root) refuseFolderWithinRoot(dir *os.File, name, refusal string) error {
	rootInfo, err := r.Stat(".")
	if err != nil {
		return fmt.Errorf("checking whether %s is inside the sync folder: %w", name, err)
	}
	rootID, ok := IdentityFromFileInfo(rootInfo)
	if !ok {
		return fmt.Errorf("checking whether %s is inside the sync folder: the sync folder's device and inode cannot be read", name)
	}

	up := "."
	var below FileIdentity
	for range maxFoldersAboveAFolder {
		st, statErr := lstatEntryAt(dir, up)
		if statErr != nil {
			return fmt.Errorf("checking whether %s is inside the sync folder: %w", name, statErr)
		}
		folder := FileIdentity{Device: statFieldToUint64(st.Dev), Inode: statFieldToUint64(st.Ino)}
		switch {
		case folder.Device == rootID.Device && folder.Inode == rootID.Inode:
			return unsafePathf("%s is inside the sync folder, %s", name, refusal)
		case folder == below:
			// The top of the filesystem is its own parent.
			return nil
		}
		below = folder
		up = filepath.Join(up, "..")
	}

	return fmt.Errorf("checking whether %s is inside the sync folder: more than %d folders above it", name, maxFoldersAboveAFolder)
}

// normalizeDescriptorFailure is normalizeNotExist for a failure of a call
// made on descriptors, which holds no root handle: it opens one for the
// check, as Rename checks on the handle it renamed through.
func (r *Root) normalizeDescriptorFailure(clean string, original error) error {
	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return localpath.JoinOnOneLine(original, fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err)))
	}

	normalized := r.normalizeNotExist(root, clean, original)
	if closeErr := root.Close(); closeErr != nil {
		return localpath.JoinOnOneLine(normalized, fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr))
	}

	return normalized
}

// cleanEntryName is cleanRelative for a name that has a parent folder inside
// the root: the sync folder itself cannot be renamed, or renamed onto.
func cleanEntryName(rel string) (string, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return "", unsafePathf("refusing to rename the sync folder itself")
	}

	return clean, nil
}

func noReplaceResult(verb, src, dst string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return fmt.Errorf("%s %s to %s: %w", verb, src, dst, ErrTargetExists)
	default:
		return fmt.Errorf("%s %s to %s: %w", verb, src, dst, err)
	}
}

func closeDirectory(dir *os.File) {
	_ = dir.Close() //nolint:errcheck // read-only directory descriptor
}

// renameNoReplaceAt moves from in fromDir to to in toDir, both inside the
// root, without replacing anything at to and without copying: the system's
// flag, or link then unlink, or, for an entry neither can move, a check that
// to is free and then a plain rename.
func (r *Root) renameNoReplaceAt(fromDir *os.File, from string, toDir *os.File, to string) error {
	linkRefused, err := r.renameOrLinkAt(fromDir, from, toDir, to)
	if !linkRefused {
		return err
	}

	return r.renameCheckedAt(fromDir, from, toDir, to)
}

// moveInNoReplaceAt moves from in fromDir, a folder outside the root, to to in
// toDir without replacing anything at to: the system's flag, or link then
// unlink, or, for a regular file neither can move, an exclusive copy. Any
// other entry fails with the flag's refusal joined to
// localpath.ErrNotRegularFile.
func (r *Root) moveInNoReplaceAt(fromDir *os.File, from string, toDir *os.File, to string) error {
	linkRefused, err := r.renameOrLinkAt(fromDir, from, toDir, to)
	if !linkRefused {
		return err
	}

	copied, copyErr := r.copyExclusiveAt(fromDir, from, toDir, to)
	if copyErr != nil {
		if errors.Is(copyErr, localpath.ErrNotRegularFile) {
			return fmt.Errorf("%w, and only a regular file can be copied: %w", err, copyErr)
		}
		return copyErr
	}

	return r.unlinkSourceHolding(fromDir, from, copied)
}

// renameOrLinkAt moves from in fromDir to to in toDir with the system's
// no-replace flag or, where the flag is refused, by linking to and unlinking
// from; either fails on an existing name. linkRefused reports that the entry
// cannot be linked (localpath.IsLinkUnsupported), so neither moved it, and
// err is then the flag's refusal, for the caller's last step to report.
func (r *Root) renameOrLinkAt(fromDir *os.File, from string, toDir *os.File, to string) (linkRefused bool, err error) {
	refused, renameErr := r.ops.renameExclusive(fromDir, from, toDir, to)
	if !refused {
		return false, renameErr
	}

	linkErr := r.ops.link(fromDir, from, toDir, to)
	switch {
	case linkErr == nil:
		return false, r.unlinkLinkedSource(fromDir, from, toDir, to)
	case localpath.IsLinkUnsupported(linkErr):
		return true, renameErr
	default:
		return false, linkErr
	}
}

// renameCheckedAt renames from in fromDir to to in toDir after a lstat of to
// on toDir finds nothing there. A plain rename that still finds to taken
// fails as the flag does: ENOTEMPTY and EEXIST match os.ErrExist already,
// and EISDIR (a file onto a folder) and ENOTDIR (a folder onto a file) mean
// the same here, since both names are base names in open folders.
func (r *Root) renameCheckedAt(fromDir *os.File, from string, toDir *os.File, to string) error {
	err := lstatAt(toDir, to)
	switch {
	case err == nil:
		return fmt.Errorf("checking %s: %w", to, os.ErrExist)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checking %s: %w", to, err)
	}

	err = r.ops.renameAt(fromDir, from, toDir, to)
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("%w: %w", os.ErrExist, err)
	}

	return err
}

// unlinkLinkedSource removes from in fromDir once its entry is linked as to
// in toDir, only while from still names that entry (unlinkSourceHolding).
// The linked entry is read on to's folder descriptor without following it;
// a read that fails, a to already gone included, leaves from alone and is
// reported as a source not removed, since from may still hold the entry.
func (r *Root) unlinkLinkedSource(fromDir *os.File, from string, toDir *os.File, to string) error {
	linked, err := r.ops.lstatEntry(toDir, to)
	if err != nil {
		return fmt.Errorf("%w: checking %s before removing %s: %w", ErrSourceNotRemoved, to, from, err)
	}

	return r.unlinkSourceHolding(fromDir, from, FileIdentity{
		Device: statFieldToUint64(linked.Dev),
		Inode:  statFieldToUint64(linked.Ino),
	})
}

// unlinkSourceHolding removes from in fromDir once the entry placed is at its
// destination, only while from still names that entry: the two are compared
// by device and inode, from read on its folder's descriptor without
// following it. A program that saved over from by renaming a new file onto
// it keeps its save, and a from already gone needs no unlink: either is the
// move done, as if the change had come a moment later. A read that fails
// another way leaves from alone and is reported as a source not removed
// (ErrSourceNotRemoved) with its reason. A save that lands between the read
// and the unlink is still removed with it.
func (r *Root) unlinkSourceHolding(fromDir *os.File, from string, placed FileIdentity) error {
	source, err := r.ops.lstatEntry(fromDir, from)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w: checking %s before removing it: %w", ErrSourceNotRemoved, from, err)
	case statFieldToUint64(source.Dev) != placed.Device || statFieldToUint64(source.Ino) != placed.Inode:
		return nil
	}

	return r.unlinkSource(fromDir, from)
}

// unlinkSource removes from in fromDir once the entry is at its destination;
// a failure is ErrSourceNotRemoved, a move that happened.
func (r *Root) unlinkSource(fromDir *os.File, from string) error {
	if err := r.ops.unlink(fromDir, from); err != nil {
		return fmt.Errorf("%w: %w", ErrSourceNotRemoved, err)
	}

	return nil
}

// lstatAt reports whether name is in dir, without following it.
func lstatAt(dir *os.File, name string) error {
	_, err := lstatEntryAt(dir, name)

	return err
}

// lstatEntryAt stats name in dir without following it.
func lstatEntryAt(dir *os.File, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := withFd(dir, func(dirfd int) error {
		return unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	})

	return st, err
}

// copyExclusiveAt creates to in toDir, failing if anything is there, copies
// the regular file from in fromDir into it with its mode and modification
// time, syncs it and then toDir, so the new name is durable before the caller
// removes the source. A folder the filesystem cannot sync is tolerated. A copy
// that fails, its folder's sync included, removes its file with unlink, and
// says so if it cannot. It returns the identity of the file it copied, read
// on the descriptor it copied from, for the caller to remove the source only
// while from still names that file.
func (r *Root) copyExclusiveAt(fromDir *os.File, from string, toDir *os.File, to string) (copied FileIdentity, err error) {
	in, err := openatNoFollow(fromDir, from)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("opening %s to copy it: %w", from, err)
	}
	defer in.Close() //nolint:errcheck // read-only source

	info, err := in.Stat()
	if err != nil {
		return FileIdentity{}, fmt.Errorf("stating %s: %w", from, err)
	}
	if !info.Mode().IsRegular() {
		return FileIdentity{}, fmt.Errorf("copying %s: %w", from, localpath.ErrNotRegularFile)
	}
	copied, ok := IdentityFromFileInfo(info)
	if !ok {
		return FileIdentity{}, fmt.Errorf("copying %s: its device and inode cannot be read", from)
	}
	out, err := openatExclusive(toDir, to, info.Mode().Perm())
	if err != nil {
		return FileIdentity{}, fmt.Errorf("creating %s: %w", to, err)
	}
	defer func() {
		if err != nil {
			err = discardPartialCopy(out, toDir, to, err, r.ops.unlink)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return FileIdentity{}, fmt.Errorf("copying %s: %w", from, err)
	}
	if err = out.Chmod(info.Mode().Perm()); err != nil {
		return FileIdentity{}, fmt.Errorf("setting the mode of %s: %w", to, err)
	}
	if err = setTimesAt(toDir, to, info.ModTime()); err != nil {
		return FileIdentity{}, fmt.Errorf("setting times on %s: %w", to, err)
	}
	if err = out.Sync(); err != nil {
		return FileIdentity{}, fmt.Errorf("syncing %s: %w", to, err)
	}
	if err = out.Close(); err != nil {
		return FileIdentity{}, fmt.Errorf("closing %s: %w", to, err)
	}
	if syncErr := r.ops.syncDirFile(toDir); syncErr != nil && !localpath.IsDirSyncUnsupported(syncErr) {
		return FileIdentity{}, fmt.Errorf("syncing the folder of %s: %w", to, syncErr)
	}

	return copied, nil
}

// discardPartialCopy closes out, the partial copy named name in dir that cause
// ended, and removes it with unlink. A leftover partial copy makes the next
// attempt find the name taken (ErrTargetExists), so a close or removal that
// fails is joined to cause; a copy already closed, or already gone, is no
// further failure.
func discardPartialCopy(
	out *os.File, dir *os.File, name string, cause error, unlink func(dir *os.File, name string) error,
) error {
	result := cause
	if err := out.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		result = localpath.JoinOnOneLine(result, fmt.Errorf("closing partial copy %s: %w", name, err))
	}
	if err := unlink(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = localpath.JoinOnOneLine(result, fmt.Errorf("removing partial copy %s: %w", name, err))
	}

	return result
}

// setTimesAt sets the access and modification times of name in dir to
// mtime, to the nanosecond, with utimensat(2) on dir's descriptor and
// AT_SYMLINK_NOFOLLOW: only the base name is resolved, inside a folder
// already open. Whatever holds name at that moment gets the times: the copy,
// or an entry swapped in for it, a symlink itself and never what it points
// to, a file or a folder with the source's modification time. Only times
// change, inside the open folder; no content moves and nothing is replaced.
func setTimesAt(dir *os.File, name string, mtime time.Time) error {
	times := []unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}

	return withFd(dir, func(dirfd int) error {
		return unix.UtimesNanoAt(dirfd, name, times, unix.AT_SYMLINK_NOFOLLOW)
	})
}

func openatExclusive(dir *os.File, name string, perm os.FileMode) (*os.File, error) {
	var fd int
	err := withFd(dir, func(dirfd int) error {
		var openErr error
		fd, openErr = unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
		return openErr //nolint:wrapcheck // the caller names the file
	})
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), name), nil
}

func renameExclusiveDescriptors(fromDir *os.File, from string, toDir *os.File, to string) (bool, error) {
	var refused bool
	err := withFd(fromDir, func(fromFd int) error {
		return withFd(toDir, func(toFd int) error {
			var renameErr error
			refused, renameErr = renameExclusiveAt(fromFd, from, toFd, to)
			return renameErr
		})
	})

	return refused, err
}

func linkDescriptors(fromDir *os.File, from string, toDir *os.File, to string) error {
	return withFd(fromDir, func(fromFd int) error {
		return withFd(toDir, func(toFd int) error {
			return unix.Linkat(fromFd, from, toFd, to, 0)
		})
	})
}

func unlinkDescriptor(dir *os.File, name string) error {
	return withFd(dir, func(fd int) error {
		return unix.Unlinkat(fd, name, 0)
	})
}
