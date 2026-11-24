// Package synctree provides a rooted filesystem capability for sync-runtime
// operations under one validated sync root. Unlike localpath, callers do not
// re-establish trust on every call; unlike fsroot, this boundary models user
// content under the sync tree rather than repo-managed state files.
package synctree

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// Handle is the contained filesystem surface every sync-tree operation runs
// through. Every method resolves names against an open root descriptor, so a
// name whose components leave the root fails instead of following the
// escape. Mutating operations are part of this interface precisely because
// they are the ones that destroy data when containment is skipped.
//
// It is exported for one purpose: Root.Interpose, the seam tests use to put
// an emulated filesystem under the sync tree (D-094).
type Handle interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	Mkdir(name string, perm os.FileMode) error
	Remove(name string) error
	RemoveAll(name string) error
	Rename(oldname, newname string) error
	Chtimes(name string, atime time.Time, mtime time.Time) error
	FS() fs.FS
	Close() error
}

// rootHandle is the package's name for Handle.
type rootHandle = Handle

type osRootHandle struct {
	root *os.Root
	// dir is the folder root was opened on: the birth time is read by path
	// (withBirthTime), which os.Root does not offer.
	dir string
}

func (h *osRootHandle) Open(name string) (*os.File, error) {
	//nolint:wrapcheck // openWithRoot adds the sync-tree boundary context.
	return h.root.Open(name)
}

func (h *osRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	//nolint:wrapcheck // openWithRoot adds the sync-tree boundary context.
	return h.root.OpenFile(name, flag, perm)
}

func (h *osRootHandle) Stat(name string) (os.FileInfo, error) {
	info, err := h.root.Stat(name)
	if err != nil {
		//nolint:wrapcheck // caller adds rooted path context after containment checks.
		return nil, err
	}

	return withBirthTime(info, filepath.Join(h.dir, name)), nil
}

func (h *osRootHandle) Lstat(name string) (os.FileInfo, error) {
	info, err := h.root.Lstat(name)
	if err != nil {
		//nolint:wrapcheck // caller adds rooted path context after containment checks.
		return nil, err
	}

	return withBirthTime(info, filepath.Join(h.dir, name)), nil
}

func (h *osRootHandle) Mkdir(name string, perm os.FileMode) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Mkdir(name, perm)
}

func (h *osRootHandle) Remove(name string) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Remove(name)
}

func (h *osRootHandle) RemoveAll(name string) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.RemoveAll(name)
}

func (h *osRootHandle) Rename(oldname, newname string) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Rename(oldname, newname)
}

// Chtimes carries the os.Root caveat documented upstream: on Unix a
// regular-file-to-symlink swap racing this call may apply to the link rather
// than its target. That is strictly narrower than the unrooted os.Chtimes it
// replaces, and the sync tree has no stronger primitive available.
func (h *osRootHandle) Chtimes(name string, atime time.Time, mtime time.Time) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Chtimes(name, atime, mtime)
}

func (h *osRootHandle) FS() fs.FS {
	return birthTimeFS{fsys: h.root.FS(), dir: h.dir}
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

// rootOps is the injection seam for failure testing. Every mutating hook
// receives an open rootHandle plus a rooted-relative name, or folders opened
// through the root plus a base name, rather than an absolute path: an
// absolute path carries no containment, so accepting one here would let a
// future call site reintroduce the escape this boundary exists to prevent.
//
// lstatAbs is the deliberate exception for the root itself. It answers
// questions about the sync root directory, which by definition sits outside
// any root handle, and must never be used for paths beneath the root. A
// place outside the root (the trash entry a file is taken or copied out to,
// the folder a restored file comes from) reaches the hooks as its folder's
// descriptor plus a base name too, the folder opened once and checked to lie
// outside the root by what it is.
type rootOps struct {
	openRoot  func(dir string) (rootHandle, error)
	remove    func(root rootHandle, name string) error
	rmdir     func(root rootHandle, name string) error
	removeAll func(root rootHandle, name string) error
	rename    func(root rootHandle, oldname, newname string) error
	chtimes   func(root rootHandle, name string, atime time.Time, mtime time.Time) error
	// renameExclusive, link, unlink and renameAt are the descriptor calls of
	// a no-replace rename and its fallbacks (rename_noreplace_unix.go), and
	// of a detach's set-aside and move out, a preserve's link, and the
	// put-back of a set-aside file (detach_unix.go); seams so tests can model
	// a system without the no-replace flag, a filesystem without hard links,
	// a move or link across filesystems, a write that lands right after a
	// rename, a source that cannot be removed, or a destination taken
	// between the check and the plain rename. renameExclusive reports refused
	// when the flag is not offered.
	renameExclusive func(fromDir *os.File, from string, toDir *os.File, to string) (refused bool, err error)
	link            func(fromDir *os.File, from string, toDir *os.File, to string) error
	unlink          func(dir *os.File, name string) error
	renameAt        func(fromDir *os.File, from string, toDir *os.File, to string) error
	// lstatEntry stats a name in a folder already open, without following
	// it: the identity check a no-replace rename's fallback makes before it
	// unlinks the source; a seam so tests can make that check fail.
	lstatEntry func(dir *os.File, name string) (unix.Stat_t, error)
	lstatAbs   func(path string) (os.FileInfo, error)
	// getRootMark and setRootMark read and write the sync folder's bind
	// token (root_identity_unix.go); a seam so tests can model filesystems
	// without extended attributes.
	getRootMark func(dir *os.File) (string, error)
	setRootMark func(dir *os.File, token string) error
	// syncDirFile flushes a folder already open: the destination folder of
	// MoveInNoReplace's exclusive copy (rename_noreplace_unix.go), and the
	// folder outside the root a detached file is copied into
	// (detach_unix.go); a seam because an fsync has no observable effect to
	// test.
	syncDirFile func(dir *os.File) error
}

func defaultRootOps() rootOps {
	return rootOps{
		openRoot: func(dir string) (rootHandle, error) {
			root, err := os.OpenRoot(dir)
			if err != nil {
				//nolint:wrapcheck // callers add the concrete sync-root path context.
				return nil, err
			}

			return &osRootHandle{root: root, dir: dir}, nil
		},
		remove: func(root rootHandle, name string) error {
			return root.Remove(name)
		},
		// rmdir keeps rmdir(2) semantics (directories only, fails on a
		// non-empty directory) so the final removal stays the race guard that
		// RemoveEmptyDirNoFollow documents. It is a separate hook from remove
		// so failure-injection tests can target that exact syscall.
		rmdir: removeEmptyDirRooted,
		removeAll: func(root rootHandle, name string) error {
			return root.RemoveAll(name)
		},
		rename: func(root rootHandle, oldname, newname string) error {
			return root.Rename(oldname, newname)
		},
		chtimes: func(root rootHandle, name string, atime time.Time, mtime time.Time) error {
			return root.Chtimes(name, atime, mtime)
		},
		renameExclusive: renameExclusiveDescriptors,
		link:            linkDescriptors,
		unlink:          unlinkDescriptor,
		renameAt:        renameat,
		lstatEntry:      lstatEntryAt,
		lstatAbs:        os.Lstat,
		getRootMark:     getRootMarkXattr,
		setRootMark:     setRootMarkXattr,
		syncDirFile:     (*os.File).Sync,
	}
}

// Root is a rooted capability for sync-runtime filesystem operations.
type Root struct {
	dir string
	ops rootOps
	// memory is the in-memory volume the root runs on (OpenMemory), nil on
	// a real folder. The operations that reach the filesystem by descriptor
	// rather than through the handle (a no-replace rename, removing a
	// verified file, detaching one out of the tree) run on it instead.
	memory *memoryVolume
}

// Interpose returns a Root over the same folder whose every rooted operation
// runs through wrap(h), h being the handle it would otherwise use. Tests use
// it to emulate other filesystems beneath the sync tree: a case-insensitive
// volume, injected errno (internal/synctest). Operations that reach the
// filesystem by descriptor after opening a parent through the handle (rmdir,
// detach, the no-replace rename) see the wrapper only for that open.
func (r *Root) Interpose(wrap func(Handle) Handle) *Root {
	ops := r.ops
	open := ops.openRoot
	ops.openRoot = func(dir string) (rootHandle, error) {
		handle, err := open(dir)
		if err != nil {
			return nil, err
		}

		return wrap(handle), nil
	}

	return &Root{dir: r.dir, ops: ops, memory: r.memory}
}

// ErrUnsafePath classifies a path the sync folder must not touch: outside the
// folder, through a non-folder, or the folder itself. Callers branch on it with
// errors.Is to tell a permanent path problem from a transient one.
var ErrUnsafePath = errors.New("path is not safe to change inside the sync folder")

// ErrUnsupportedTreeEntry classifies an entry the rooted tree cannot operate
// on, such as a symbolic link or a device file.
var ErrUnsupportedTreeEntry = errors.New("unsupported entry in the sync folder")

// treeRefusalError is a refusal classified by one of the sentinels above. Its
// message is the detail alone ("a/b is a symbolic link"), and it has no
// Unwrap, so a renderer that reports the innermost error shows what is wrong
// with which path instead of the bare sentinel text. errors.Is still matches
// the sentinel through Is.
type treeRefusalError struct {
	kind   error
	detail string
}

func (e *treeRefusalError) Error() string { return e.detail }

func (e *treeRefusalError) Is(target error) bool { return target == e.kind }

func unsafePathf(format string, args ...any) error {
	return &treeRefusalError{kind: ErrUnsafePath, detail: fmt.Sprintf(format, args...)}
}

func unsupportedEntryf(format string, args ...any) error {
	return &treeRefusalError{kind: ErrUnsupportedTreeEntry, detail: fmt.Sprintf(format, args...)}
}

type PathState struct {
	Exists    bool
	IsDir     bool
	IsSymlink bool
}

type rootedTreeEntryKind string

const (
	rootedTreeEntryDir  rootedTreeEntryKind = "dir"
	rootedTreeEntryFile rootedTreeEntryKind = "file"
)

type rootedTreeEntry struct {
	kind rootedTreeEntryKind
	size int64
	hash [sha256.Size]byte
}

// Open establishes a rooted sync-tree capability for dir.
func Open(dir string) (*Root, error) {
	if dir == "" {
		return nil, errors.New("sync folder path is empty")
	}

	clean := filepath.Clean(dir)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, fmt.Errorf("resolving sync folder %s: %w", clean, err)
	}

	return newRoot(abs), nil
}

func newRoot(dir string) *Root {
	return &Root{
		dir: dir,
		ops: defaultRootOps(),
	}
}

// Path returns the absolute sync-root path backing this capability.
func (r *Root) Path() string {
	return r.dir
}

// cleanRelative rejects paths that would leave the tree. Both refusals wrap
// ErrUnsafePath: containment failures are the definition of an unsafe path,
// and callers classify on that sentinel to tell a permanent path problem from
// a transient one.
func cleanRelative(path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	if filepath.IsAbs(path) {
		return "", unsafePathf("%q is an absolute path, not a path inside the sync folder", path)
	}

	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", unsafePathf("%q points outside the sync folder", path)
	}
	if clean == "." {
		return ".", nil
	}

	return clean, nil
}

func (r *Root) relativeFromAbs(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}

	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return "", fmt.Errorf("resolving path %s: %w", clean, err)
		}

		clean = abs
	}

	rel, err := filepath.Rel(r.dir, clean)
	if err != nil {
		return "", fmt.Errorf("relativizing %s to %s: %w", clean, r.dir, err)
	}

	return cleanRelative(rel)
}

// Abs returns the absolute path for rel within the sync root.
func (r *Root) Abs(rel string) (string, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return r.dir, nil
	}

	return filepath.Join(r.dir, clean), nil
}

// Rel returns the rooted relative path for abs. It rejects paths outside the root.
func (r *Root) Rel(abs string) (string, error) {
	return r.relativeFromAbs(abs)
}

func (r *Root) openWithRoot(
	rel string,
	opener func(root rootHandle, clean string) (*os.File, error),
) (*os.File, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	file, openErr := opener(root, clean)
	openErr = r.normalizeNotExist(root, clean, openErr)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, localpath.JoinOnOneLine(openErr, closeErr)
		}

		return nil, fmt.Errorf("opening %s: %w", r.nameForMessage(clean), openErr)
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, localpath.JoinOnOneLine(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr)
	}

	return file, nil
}

func (r *Root) Open(rel string) (*os.File, error) {
	file, err := r.openWithRoot(rel, func(root rootHandle, clean string) (*os.File, error) {
		return root.Open(clean)
	})
	if err != nil {
		return nil, err
	}

	return file, nil
}

func (r *Root) OpenAbs(abs string) (*os.File, error) {
	rel, err := r.Rel(abs)
	if err != nil {
		return nil, err
	}

	return r.Open(rel)
}

func (r *Root) OpenFile(rel string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := r.openWithRoot(rel, func(root rootHandle, clean string) (*os.File, error) {
		return root.OpenFile(clean, flag, perm)
	})
	if err != nil {
		return nil, err
	}

	return file, nil
}

func (r *Root) Stat(rel string) (os.FileInfo, error) {
	return r.statWithRoot(rel, "stating %s", func(root rootHandle, clean string) (os.FileInfo, error) {
		return root.Stat(clean)
	})
}

func (r *Root) statWithRoot(
	rel string,
	errorFormat string,
	stat func(root rootHandle, clean string) (os.FileInfo, error),
) (os.FileInfo, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	info, statErr := stat(root, clean)
	statErr = r.normalizeNotExist(root, clean, statErr)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, localpath.JoinOnOneLine(statErr, closeErr)
		}

		return nil, fmt.Errorf(errorFormat+": %w", r.nameForMessage(clean), statErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr)
	}

	return info, nil
}

// mutateWithRoot runs a mutating operation against a freshly opened root
// handle. It mirrors openWithRoot/statWithRoot so every sync-tree side effect
// resolves its name inside the root rather than against a lexically joined
// absolute path.
func (r *Root) mutateWithRoot(
	rel string,
	errorFormat string,
	mutate func(root rootHandle, clean string) error,
) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	mutateErr := mutate(root, clean)
	mutateErr = r.normalizeNotExist(root, clean, mutateErr)
	closeErr := root.Close()
	if mutateErr != nil {
		if closeErr != nil {
			return localpath.JoinOnOneLine(mutateErr, closeErr)
		}

		return fmt.Errorf(errorFormat+": %w", r.nameForMessage(clean), mutateErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr)
	}

	return nil
}

// nameForMessage is how error text names a rooted-relative path: relative to
// the sync folder, as everything the user sees does, with the sync folder
// itself named by its full path.
func (r *Root) nameForMessage(clean string) string {
	if clean == "." {
		return r.dir
	}

	return clean
}

func (r *Root) StatAbs(abs string) (os.FileInfo, error) {
	rel, err := r.Rel(abs)
	if err != nil {
		return nil, err
	}

	return r.Stat(rel)
}

func (r *Root) Lstat(rel string) (os.FileInfo, error) {
	return r.statWithRoot(rel, "stating %s without following links", func(root rootHandle, clean string) (os.FileInfo, error) {
		return root.Lstat(clean)
	})
}

func (r *Root) PathStateNoFollow(rel string) (PathState, error) {
	info, err := r.Lstat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PathState{}, nil
		}

		return PathState{}, err
	}

	isSymlink := info.Mode()&os.ModeSymlink != 0
	return PathState{
		Exists:    true,
		IsDir:     info.IsDir() && !isSymlink,
		IsSymlink: isSymlink,
	}, nil
}

func (r *Root) ReadDir(rel string) ([]os.DirEntry, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	entries, readErr := fs.ReadDir(root.FS(), clean)
	readErr = r.normalizeNotExist(root, clean, readErr)
	closeErr := root.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, localpath.JoinOnOneLine(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading directory %s: %w", r.nameForMessage(clean), readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr)
	}

	return entries, nil
}

func (r *Root) ReadDirAbs(abs string) ([]os.DirEntry, error) {
	rel, err := r.Rel(abs)
	if err != nil {
		return nil, err
	}

	return r.ReadDir(rel)
}

// DirEmptyNoFollow reports whether rel is an empty directory. The final path
// and every discovered child are inspected with Lstat so projection callers do
// not follow symlinks while deciding whether a mount-root move is safe.
func (r *Root) DirEmptyNoFollow(rel string) (bool, error) {
	info, err := r.Lstat(rel)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, unsafePathf("%s is not a folder", rel)
	}

	entries, err := r.ReadDir(rel)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}

	for _, entry := range entries {
		child := filepath.Join(rel, entry.Name())
		info, err := r.Lstat(child)
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, unsupportedEntryf("%s is a symbolic link", child)
		}
	}

	return false, nil
}

// TreesEqualNoFollow compares two directory trees without following symlinks.
// It intentionally compares only directory structure plus regular-file size and
// SHA-256 content. Metadata such as mtime, mode, owner, and xattrs are outside
// this safety check.
func (r *Root) TreesEqualNoFollow(leftRel string, rightRel string) (bool, error) {
	left, err := r.rootedTreeManifestNoFollow(leftRel)
	if err != nil {
		return false, fmt.Errorf("reading tree %s: %w", leftRel, err)
	}
	right, err := r.rootedTreeManifestNoFollow(rightRel)
	if err != nil {
		return false, fmt.Errorf("reading tree %s: %w", rightRel, err)
	}
	if len(left) != len(right) {
		return false, nil
	}

	for rel, leftEntry := range left {
		rightEntry, ok := right[rel]
		if !ok || leftEntry != rightEntry {
			return false, nil
		}
	}

	return true, nil
}

// ValidateTreeNoFollow verifies that rel is a directory tree made only of
// directories and regular files. It walks with Lstat so projection callers can
// reject symlinks and unsupported entries before moving mount infrastructure.
func (r *Root) ValidateTreeNoFollow(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}

	if err := r.validateTreeNoFollow(clean); err != nil {
		return fmt.Errorf("validating tree %s: %w", rel, err)
	}

	return nil
}

func (r *Root) validateTreeNoFollow(rel string) error {
	info, err := r.Lstat(rel)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return unsupportedEntryf("%s is a symbolic link", rel)
	case info.IsDir():
		children, err := r.ReadDir(rel)
		if err != nil {
			return err
		}
		sort.Slice(children, func(i, j int) bool {
			return children[i].Name() < children[j].Name()
		})
		for _, child := range children {
			if err := r.validateTreeNoFollow(filepath.Join(rel, child.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		return nil
	default:
		return unsupportedEntryf("%s is not a regular file or folder (mode %s)", rel, info.Mode())
	}
}

func (r *Root) rootedTreeManifestNoFollow(baseRel string) (map[string]rootedTreeEntry, error) {
	base, err := cleanRelative(baseRel)
	if err != nil {
		return nil, err
	}

	rootInfo, err := r.Lstat(base)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, unsafePathf("%s is not a folder", baseRel)
	}

	entries := make(map[string]rootedTreeEntry)
	if err := r.appendRootedTreeManifest(base, ".", entries); err != nil {
		return nil, err
	}

	return entries, nil
}

func (r *Root) appendRootedTreeManifest(baseRel string, rel string, entries map[string]rootedTreeEntry) error {
	currentRel := baseRel
	if rel != "." {
		currentRel = filepath.Join(baseRel, rel)
	}

	children, err := r.ReadDir(currentRel)
	if err != nil {
		return err
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name() < children[j].Name()
	})

	for _, child := range children {
		childRel := filepath.Join(rel, child.Name())
		rootedChildRel := filepath.Join(baseRel, childRel)
		info, err := r.Lstat(rootedChildRel)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return unsupportedEntryf("%s is a symbolic link", rootedChildRel)
		case info.IsDir():
			entries[childRel] = rootedTreeEntry{kind: rootedTreeEntryDir}
			if err := r.appendRootedTreeManifest(baseRel, childRel, entries); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			hash, err := r.hashRegularFile(rootedChildRel)
			if err != nil {
				return err
			}
			entries[childRel] = rootedTreeEntry{
				kind: rootedTreeEntryFile,
				size: info.Size(),
				hash: hash,
			}
		default:
			return unsupportedEntryf("%s is not a regular file or folder (mode %s)", rootedChildRel, info.Mode())
		}
	}

	return nil
}

func (r *Root) hashRegularFile(rel string) ([sha256.Size]byte, error) {
	file, err := r.Open(rel)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close() //nolint:errcheck // the file is opened read-only for hashing; there is nothing buffered to flush

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hashing %s: %w", rel, err)
	}

	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

// MkdirExact creates the folder rel inside the sync root, and any parent it
// is missing, and returns the name the folder has on disk. It never adopts
// an entry it did not create: the parents are made or found as
// MkdirAllNoFollow makes them (real folders, never a symbolic link), and the
// folder itself is made with mkdir, so a volume that folds case or Unicode
// form answers EEXIST where another entry already holds an equivalent name.
// Only then is the parent listed, and the entry adopted is the one spelled
// exactly so, else the only one whose stored (NFC) form is rel's
// (namekey.FindStored): the folder a crashed create left behind, or the NFD
// spelling of the stored name (R-2.13.2). Anything else at the name (a case
// variant, two entries of one stored form, a file, a symbolic link) fails
// the create with ErrTargetExists and is left as it is (R-2.12.10).
//
// On a volume that stores names byte for byte, an NFD-named folder does not
// make mkdir of the NFC name fail: the folder is then created beside it, a
// local collision observation reports (R-2.12.3), never an adoption.
//
// Every folder below the sync root is a folder in the user's tree, so the
// mode is not the caller's to choose: each new folder asks for
// localpath.UserTreeDirPerm and gets the umask (R-5.10.2). An existing
// folder keeps its mode.
func (r *Root) MkdirExact(rel string) (string, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return "", unsafePathf("the sync folder itself is not a folder to create inside it")
	}

	root, err := r.openRootNoFollow()
	if err != nil {
		return "", err
	}
	defer root.Close() //nolint:errcheck // rooted handle release; mutation errors are reported by the operation itself

	parent := filepath.Dir(clean)
	if parent != "." {
		current := ""
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			if err := ensureNoFollowDirectory(root, current); err != nil {
				return "", err
			}
		}
	}

	mkdirErr := root.Mkdir(clean, localpath.UserTreeDirPerm)
	if mkdirErr == nil {
		return clean, nil
	}
	if !errors.Is(mkdirErr, os.ErrExist) {
		return "", fmt.Errorf("creating directory %s: %w", clean, r.normalizeNotExist(root, clean, mkdirErr))
	}

	return adoptExistingFolder(root, clean)
}

// adoptExistingFolder is MkdirExact's answer when mkdir found the name
// taken: the name on disk of the folder that is clean, or ErrTargetExists.
func adoptExistingFolder(root rootHandle, clean string) (string, error) {
	parent := filepath.Dir(clean)
	base := filepath.Base(clean)
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(parent))
	if err != nil {
		return "", fmt.Errorf("listing %s for the folder %s: %w", parent, clean, err)
	}

	name, found := entryOfName(entries, base)
	if !found {
		return "", fmt.Errorf("creating directory %s: another entry holds the name: %w", clean, ErrTargetExists)
	}
	onDisk := name
	if parent != "." {
		onDisk = filepath.Join(parent, name)
	}

	info, err := root.Lstat(onDisk)
	if err != nil {
		return "", fmt.Errorf("checking directory %s: %w", onDisk, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("creating directory %s: %s is not a folder: %w", clean, onDisk, ErrTargetExists)
	}

	return onDisk, nil
}

// entryOfName is the name of the entry of a folder listing that is base:
// the one spelled exactly so, else the only one with base's stored (NFC)
// form. A case variant is another name, and two entries of one stored form
// are a local collision (R-2.12.3): neither is base.
func entryOfName(entries []fs.DirEntry, base string) (string, bool) {
	for _, entry := range entries {
		if entry.Name() == base {
			return base, true
		}
	}
	match := namekey.FindStored(entries, namekey.Stored(base), func(entry *fs.DirEntry) string { return (*entry).Name() })
	if !match.Found() {
		return "", false
	}

	return entries[match.Index].Name(), true
}

// HoldsName reports whether rel's folder holds an entry that is rel's final
// name: spelled exactly so, or the only entry of its stored (NFC) form. An
// lstat cannot tell: on a volume that folds case it finds a case variant,
// which is another name. A missing folder holds nothing.
func (r *Root) HoldsName(rel string) (bool, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return false, err
	}
	if clean == "." {
		return true, nil
	}
	entries, err := r.ReadDir(filepath.Dir(clean))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, found := entryOfName(entries, filepath.Base(clean))

	return found, nil
}

// ValidateNoSymlinkAncestors refuses rel when any parent component is a
// symbolic link or not a directory. The refusal names which, because "is a
// symbolic link" is the everyday case under follow_symlinks and reads very
// differently from a file standing where a folder should be.
func (r *Root) ValidateNoSymlinkAncestors(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	if clean == "." {
		return nil
	}
	parent := filepath.Dir(clean)
	if parent == "." {
		return r.validateRootDirectoryNoFollow()
	}

	root, err := r.openRootNoFollow()
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // rooted handle release; mutation errors are reported by the operation itself

	current := ""
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := root.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("checking parent folder %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return unsafePathf("parent folder %s is a symbolic link", current)
		}
		if !info.IsDir() {
			return unsafePathf("parent folder %s is not a folder", current)
		}
	}

	return nil
}

// MkdirAllNoFollow creates rel and any missing parents inside the sync root,
// refusing any component that is a symbolic link or not a folder, even one
// that stays inside the root. Like MkdirExact, each new folder asks for
// localpath.UserTreeDirPerm (R-5.10.2) and an existing one keeps its mode.
func (r *Root) MkdirAllNoFollow(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}

	root, err := r.openRootNoFollow()
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // rooted handle release; mutation errors are reported by the operation itself

	if clean == "." {
		return nil
	}

	current := ""
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if err := ensureNoFollowDirectory(root, current); err != nil {
			return err
		}
	}

	return nil
}

// Remove deletes a single entry inside the sync root. Paths that resolve
// outside the root fail closed.
func (r *Root) Remove(rel string) error {
	return r.mutateWithRoot(rel, "removing %s", func(root rootHandle, clean string) error {
		return r.ops.remove(root, clean)
	})
}

// RemoveEmptyDirNoFollow removes rel only if it is an empty directory within
// the rooted sync tree. The explicit empty check gives callers a clear contract;
// the final rooted removal is the race guard if the directory gains a child or
// is swapped for a non-directory after the check.
func (r *Root) RemoveEmptyDirNoFollow(rel string) error {
	empty, err := r.DirEmptyNoFollow(rel)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("removing empty directory %s: directory is not empty", rel)
	}

	if err := r.mutateWithRoot(rel, "removing empty directory %s", func(root rootHandle, clean string) error {
		return r.ops.rmdir(root, clean)
	}); err != nil {
		return err
	}

	return nil
}

// RemoveAll deletes rel and its descendants inside the sync root. Paths that
// resolve outside the root fail closed. It checks nothing about what it
// removes, so sync calls it only for bundled junk a folder delete has found
// disposable all the way down; a user's tree leaves by the verified local
// delete, one entry at a time.
func (r *Root) RemoveAll(rel string) error {
	return r.mutateWithRoot(rel, "removing tree %s", func(root rootHandle, clean string) error {
		return r.ops.removeAll(root, clean)
	})
}

func (r *Root) RemoveAbs(abs string) error {
	rel, err := r.Rel(abs)
	if err != nil {
		return err
	}

	return r.Remove(rel)
}

// Rename moves srcRel to dstRel inside the sync root. Either path resolving
// outside the root fails closed, which matters more here than elsewhere:
// rename silently replaces its destination, so an escaping rename destroys
// data the sync tree never owned.
func (r *Root) Rename(srcRel, dstRel string) error {
	srcClean, err := cleanRelative(srcRel)
	if err != nil {
		return err
	}
	dstClean, err := cleanRelative(dstRel)
	if err != nil {
		return err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	renameErr := r.ops.rename(root, srcClean, dstClean)
	renameErr = r.normalizeNotExist(root, srcClean, renameErr)
	closeErr := root.Close()
	if renameErr != nil {
		if closeErr != nil {
			return localpath.JoinOnOneLine(renameErr, closeErr)
		}

		return fmt.Errorf("renaming %s to %s: %w", r.nameForMessage(srcClean), r.nameForMessage(dstClean), renameErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing sync folder %s: %w", r.dir, closeErr)
	}

	return nil
}

func (r *Root) SameFile(srcRel, dstRel string) (bool, error) {
	// Stat already names the path; wrapping again would say it twice.
	srcInfo, err := r.Stat(srcRel)
	if err != nil {
		return false, err
	}
	dstInfo, err := r.Stat(dstRel)
	if err != nil {
		return false, err
	}

	return SameFile(srcInfo, dstInfo), nil
}

// RenameWithTemporarySibling renames srcRel to dstRel through a temporary
// sibling of dstRel (tempStem, then tempStem-1 and so on, attempts names),
// for a rename the filesystem would take as a no-op: a spelling-only rename
// on a case-insensitive volume, where dstRel names the source itself until
// the first step moves it away. Both steps are RenameNoReplace, so neither
// replaces or copies anything. The first moves on to the next temporary name
// when one is taken. When the second fails, the entry is put back under
// srcRel and the failure is returned, matching ErrTargetExists when dstRel
// was taken. On a volume that folds case srcRel and dstRel are one name, so
// whatever took dstRel holds srcRel too and the put-back fails: the entry
// then stays under the temporary name, and the error says it is kept there
// and names its path, for the user to rename it. When a lstat through the
// root does not find the temporary name either, the error says the entry
// was found under neither name instead.
//
// A step whose fallback put the entry at its destination but could not
// remove the name it came from (ErrSourceNotRemoved) has moved the entry, so
// the sequence goes on from there. When the entry reaches dstRel, a name
// left behind makes the result match ErrSourceNotRemoved, naming each on one
// line: the rename happened. A name left behind on the way to a failure is
// named in the failure, which then does not match ErrSourceNotRemoved.
func (r *Root) RenameWithTemporarySibling(srcRel, dstRel, tempStem string, attempts int) error {
	if attempts <= 0 {
		attempts = 1
	}
	if tempStem == "" {
		return errors.New("temporary rename stem is empty")
	}

	dstClean, err := cleanRelative(dstRel)
	if err != nil {
		return err
	}
	tempRel, firstErr := r.renameToTemporarySibling(srcRel, filepath.Dir(dstClean), tempStem, attempts)
	if firstErr != nil && !errors.Is(firstErr, ErrSourceNotRemoved) {
		return firstErr
	}

	secondErr := r.RenameNoReplace(tempRel, dstRel)
	if secondErr != nil && !errors.Is(secondErr, ErrSourceNotRemoved) {
		return r.putBackFromTemporarySibling(tempRel, srcRel, secondErr, firstErr)
	}

	return localpath.JoinOnOneLine(firstErr, secondErr)
}

// putBackFromTemporarySibling moves the entry at tempRel back to srcRel after
// the second step of RenameWithTemporarySibling failed with stepErr, and
// returns stepErr with what became of the entry. firstLeft, when set, is the
// first step's ErrSourceNotRemoved; it and a put-back that left tempRel are
// named as text, so the result matches stepErr only. A put-back that failed
// says the entry is under neither name only when a lstat through the root
// finds tempRel missing; otherwise it says the entry is kept as tempRel, with
// the lstat's reason when the lstat failed. The put-back's own failure names
// srcRel, so the text around it does not.
func (r *Root) putBackFromTemporarySibling(tempRel, srcRel string, stepErr, firstLeft error) error {
	result := stepErr
	if firstLeft != nil {
		result = fmt.Errorf("%w; %s was left too: %s", result, srcRel, firstLeft.Error())
	}

	putBackErr := r.RenameNoReplace(tempRel, srcRel)
	switch {
	case putBackErr == nil:
		return result
	case errors.Is(putBackErr, ErrSourceNotRemoved):
		return fmt.Errorf("%w; it is back as %s, and %s was left: %s", result, srcRel, tempRel, putBackErr.Error())
	}

	kept := filepath.Join(r.dir, tempRel)
	_, lstatErr := r.Lstat(tempRel)
	switch {
	case lstatErr == nil:
		return fmt.Errorf("%w; it is kept as %s, because putting it back failed: %s", result, kept, putBackErr.Error())
	case errors.Is(lstatErr, os.ErrNotExist):
		return fmt.Errorf("%w; it was found under neither name: putting it back failed: %s, and %s",
			result, putBackErr.Error(), lstatErr.Error())
	default:
		return fmt.Errorf("%w; it is kept as %s, because putting it back failed: %s, and %s",
			result, kept, putBackErr.Error(), lstatErr.Error())
	}
}

// renameToTemporarySibling moves srcRel to the first free temporary name in
// parent and returns that name, with an error matching ErrSourceNotRemoved
// when the entry is there but srcRel could not be removed.
func (r *Root) renameToTemporarySibling(srcRel, parent, tempStem string, attempts int) (string, error) {
	for i := range attempts {
		name := tempStem
		if i > 0 {
			name = fmt.Sprintf("%s-%d", tempStem, i)
		}
		candidate := filepath.Join(parent, name)
		err := r.RenameNoReplace(srcRel, candidate)
		if err == nil || errors.Is(err, ErrSourceNotRemoved) {
			return candidate, err
		}
		if !errors.Is(err, ErrTargetExists) {
			return "", err
		}
	}

	return "", fmt.Errorf("temporary rename path already exists under %s", parent)
}

// Chtimes sets access and modification times inside the sync root. See the
// osRootHandle.Chtimes note for the upstream Unix symlink-swap caveat.
func (r *Root) Chtimes(rel string, atime time.Time, mtime time.Time) error {
	return r.mutateWithRoot(rel, "setting times on %s", func(root rootHandle, clean string) error {
		return r.ops.chtimes(root, clean, atime, mtime)
	})
}

// SyncDir flushes a directory inside the sync root to stable storage, so a
// rename into it survives a power cut.
func (r *Root) SyncDir(rel string) error {
	dir, err := r.Open(rel)
	if err != nil {
		return err
	}

	syncErr := dir.Sync()
	closeErr := dir.Close()

	if syncErr != nil {
		return fmt.Errorf("syncing directory %s: %w", rel, syncErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing directory %s: %w", rel, closeErr)
	}

	return nil
}

// WalkDir walks the sync tree and calls fn with absolute paths rooted under r.
func (r *Root) WalkDir(fn fs.WalkDirFunc) error {
	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}
	defer root.Close() //nolint:errcheck // rooted handle release; mutation errors are reported by the operation itself

	if err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		absPath := r.dir
		if rel != "." {
			absPath = filepath.Join(r.dir, filepath.FromSlash(rel))
		}

		return fn(absPath, d, walkErr)
	}); err != nil {
		return fmt.Errorf("walking sync folder %s: %w", r.dir, err)
	}

	return nil
}

// normalizeNotExist reports original, the failure of an operation on clean
// through root, as os.ErrNotExist when clean is absent, and returns every
// other failure as it is. root must still be open.
//
// A symlinked or non-folder ancestor comes first: a positive ErrUnsafePath
// from ValidateNoSymlinkAncestors replaces the failure, because "parent folder
// alias is a symbolic link" says what a bare path-escape error does not. An
// ancestor walk that fails for another reason does not mask the caller's
// error.
//
// Then two answers have to agree. The failure itself must carry the errno of
// a missing name (localpath.IsMissingName): a create always targets a missing
// name, so a full disk (ENOSPC), a read-only mount (EROFS), a denied folder
// (EACCES) or a path escape would otherwise read as "does not exist", which
// hides each from the handling it needs. And an lstat of clean through the
// same rooted handle must find it missing too, because ENOTDIR also answers
// an operation that wanted a folder and found a file, and that name is there.
// The lstat resolves through the root, so beneath a link out of the root it
// fails with the escape instead of answering about the place outside: a
// containment failure never turns into "nothing is here", which callers read
// as permission to create.
func (r *Root) normalizeNotExist(root rootHandle, clean string, original error) error {
	if original == nil {
		return nil
	}

	if clean != "." {
		if ancestorErr := r.ValidateNoSymlinkAncestors(clean); errors.Is(ancestorErr, ErrUnsafePath) {
			return ancestorErr
		}
	}

	if !localpath.IsMissingName(original) {
		return original
	}
	if _, statErr := root.Lstat(clean); localpath.IsMissingName(statErr) {
		return os.ErrNotExist
	}

	return original
}

// normalizeRootNotExist is normalizeNotExist for the sync folder itself,
// which no rooted handle contains: the second answer is an lstat of its path
// (lstatAbs), the one name this package resolves by path.
func (r *Root) normalizeRootNotExist(original error) error {
	if !localpath.IsMissingName(original) {
		return original
	}
	if _, statErr := r.ops.lstatAbs(r.dir); localpath.IsMissingName(statErr) {
		return os.ErrNotExist
	}

	return original
}

func (r *Root) validateRootDirectoryNoFollow() error {
	// The lstat is itself the second answer normalizeRootNotExist asks for.
	info, err := r.ops.lstatAbs(r.dir)
	if localpath.IsMissingName(err) {
		return fmt.Errorf("checking sync folder %s: %w", r.dir, os.ErrNotExist)
	}
	if err != nil {
		return fmt.Errorf("checking sync folder %s: %w", r.dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return unsafePathf("sync folder %s is not a folder", r.dir)
	}

	return nil
}

func (r *Root) openRootNoFollow() (rootHandle, error) {
	if err := r.validateRootDirectoryNoFollow(); err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync folder %s: %w", r.dir, r.normalizeRootNotExist(err))
	}

	return root, nil
}

// ensureNoFollowDirectory makes sure rel is a real folder, creating it with
// the user-tree folder mode when it is missing.
func ensureNoFollowDirectory(root rootHandle, rel string) error {
	info, err := root.Lstat(rel)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return unsafePathf("%s is not a folder", rel)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking directory %s: %w", rel, err)
	}
	if err := root.Mkdir(rel, localpath.UserTreeDirPerm); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := root.Lstat(rel)
			if statErr != nil {
				return fmt.Errorf("checking directory %s after create race: %w", rel, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return unsafePathf("%s is not a folder", rel)
			}
			return nil
		}
		return fmt.Errorf("creating directory %s: %w", rel, err)
	}

	return nil
}
