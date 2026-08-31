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
)

// rootHandle is the contained filesystem surface every sync-tree operation
// runs through. Every method resolves names against an open root descriptor,
// so a name whose components leave the root fails instead of following the
// escape. Mutating operations are part of this interface precisely because
// they are the ones that destroy data when containment is skipped.
type rootHandle interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	Mkdir(name string, perm os.FileMode) error
	MkdirAll(name string, perm os.FileMode) error
	Remove(name string) error
	RemoveAll(name string) error
	Rename(oldname, newname string) error
	Chtimes(name string, atime time.Time, mtime time.Time) error
	FS() fs.FS
	Close() error
}

type osRootHandle struct {
	root *os.Root
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
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Stat(name)
}

func (h *osRootHandle) Lstat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Lstat(name)
}

func (h *osRootHandle) Mkdir(name string, perm os.FileMode) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.Mkdir(name, perm)
}

func (h *osRootHandle) MkdirAll(name string, perm os.FileMode) error {
	//nolint:wrapcheck // caller adds rooted path context after containment checks.
	return h.root.MkdirAll(name, perm)
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
	return h.root.FS()
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

// rootOps is the injection seam for failure testing. Every mutating hook
// receives an open rootHandle plus a rooted-relative name rather than an
// absolute path: an absolute path carries no containment, so accepting one
// here would let a future call site reintroduce the escape this boundary
// exists to prevent.
//
// lstatAbs is the single deliberate exception. It answers questions about the
// sync root directory itself, which by definition sits outside any root
// handle, and must never be used for paths beneath the root.
type rootOps struct {
	openRoot  func(dir string) (rootHandle, error)
	mkdirAll  func(root rootHandle, name string, perm os.FileMode) error
	remove    func(root rootHandle, name string) error
	rmdir     func(root rootHandle, name string) error
	removeAll func(root rootHandle, name string) error
	rename    func(root rootHandle, oldname, newname string) error
	chtimes   func(root rootHandle, name string, atime time.Time, mtime time.Time) error
	lstatAbs  func(path string) (os.FileInfo, error)
}

func defaultRootOps() rootOps {
	return rootOps{
		openRoot: func(dir string) (rootHandle, error) {
			root, err := os.OpenRoot(dir)
			if err != nil {
				//nolint:wrapcheck // callers add the concrete sync-root path context.
				return nil, err
			}

			return &osRootHandle{root: root}, nil
		},
		mkdirAll: func(root rootHandle, name string, perm os.FileMode) error {
			return root.MkdirAll(name, perm)
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
		lstatAbs: os.Lstat,
	}
}

// Root is a rooted capability for sync-runtime filesystem operations.
type Root struct {
	dir string
	ops rootOps
}

var ErrUnsafePath = errors.New("sync tree path is unsafe")

var ErrUnsupportedTreeEntry = errors.New("sync tree entry is unsupported")

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
		return nil, fmt.Errorf("sync root is empty")
	}

	clean := filepath.Clean(dir)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, fmt.Errorf("resolving sync root %s: %w", clean, err)
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

func cleanRelative(path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("relative path %q must not be absolute", path)
	}

	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative path %q escapes root", path)
	}
	if clean == "." {
		return ".", nil
	}

	return clean, nil
}

func (r *Root) relativeFromAbs(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
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
		return nil, fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	file, openErr := opener(root, clean)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		target := r.dir
		if clean != "." {
			target = filepath.Join(r.dir, clean)
		}

		return nil, r.normalizeNotExist(target, openErr)
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, errors.Join(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing sync root %s: %w", r.dir, closeErr)
	}

	return file, nil
}

func (r *Root) Open(rel string) (*os.File, error) {
	file, err := r.openWithRoot(rel, func(root rootHandle, clean string) (*os.File, error) {
		f, openErr := root.Open(clean)
		if openErr != nil {
			return nil, fmt.Errorf("opening %s: %w", clean, openErr)
		}

		return f, nil
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
		f, openErr := root.OpenFile(clean, flag, perm)
		if openErr != nil {
			return nil, fmt.Errorf("opening %s: %w", clean, openErr)
		}

		return f, nil
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
		return nil, fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	info, statErr := stat(root, clean)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		target := r.dir
		if clean != "." {
			target = filepath.Join(r.dir, clean)
		}

		return nil, fmt.Errorf(errorFormat+": %w", target, r.normalizeNotExist(target, statErr))
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing sync root %s: %w", r.dir, closeErr)
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
		return fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	mutateErr := mutate(root, clean)
	closeErr := root.Close()
	if mutateErr != nil {
		if closeErr != nil {
			return errors.Join(mutateErr, closeErr)
		}

		return fmt.Errorf(errorFormat+": %w", r.absForMessage(clean), r.normalizeNotExist(r.absForMessage(clean), mutateErr))
	}

	if closeErr != nil {
		return fmt.Errorf("closing sync root %s: %w", r.dir, closeErr)
	}

	return nil
}

// absForMessage renders a rooted-relative name as an absolute path for error
// text only. It is never used to perform filesystem work.
func (r *Root) absForMessage(clean string) string {
	if clean == "." {
		return r.dir
	}

	return filepath.Join(r.dir, clean)
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
		return nil, fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	entries, readErr := fs.ReadDir(root.FS(), clean)
	closeErr := root.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		target := r.dir
		if clean != "." {
			target = filepath.Join(r.dir, clean)
		}

		return nil, fmt.Errorf("reading directory %s: %w", target, r.normalizeNotExist(target, readErr))
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing sync root %s: %w", r.dir, closeErr)
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
		return false, fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, rel)
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
			return false, fmt.Errorf("%w: symlink %s", ErrUnsupportedTreeEntry, child)
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
		return fmt.Errorf("%w: symlink %s", ErrUnsupportedTreeEntry, rel)
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
		return fmt.Errorf("%w: %s has mode %s", ErrUnsupportedTreeEntry, rel, info.Mode())
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
		return nil, fmt.Errorf("%w: tree root %s is not a directory", ErrUnsafePath, baseRel)
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
			return fmt.Errorf("%w: symlink %s", ErrUnsupportedTreeEntry, rootedChildRel)
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
			return fmt.Errorf("%w: %s has mode %s", ErrUnsupportedTreeEntry, rootedChildRel, info.Mode())
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

// RemoveTreeNoFollow removes rel and its descendants without following
// symlinks. It rejects the root itself and any unsupported entry encountered
// while walking the tree.
func (r *Root) RemoveTreeNoFollow(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	if clean == "." {
		return fmt.Errorf("%w: refusing to remove sync root", ErrUnsafePath)
	}

	if err := r.removeTreeNoFollow(clean); err != nil {
		return fmt.Errorf("removing rooted tree %s: %w", rel, err)
	}

	return nil
}

func (r *Root) removeTreeNoFollow(rel string) error {
	info, err := r.Lstat(rel)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: symlink %s", ErrUnsupportedTreeEntry, rel)
	case info.IsDir():
		children, err := r.ReadDir(rel)
		if err != nil {
			return err
		}
		sort.Slice(children, func(i, j int) bool {
			return children[i].Name() < children[j].Name()
		})
		for _, child := range children {
			if err := r.removeTreeNoFollow(filepath.Join(rel, child.Name())); err != nil {
				return err
			}
		}
		return r.Remove(rel)
	case info.Mode().IsRegular():
		return r.Remove(rel)
	default:
		return fmt.Errorf("%w: %s has mode %s", ErrUnsupportedTreeEntry, rel, info.Mode())
	}
}

// MkdirAll creates rel and any missing parents inside the sync root.
// Components that resolve outside the root fail closed. Symlinks that stay
// inside the root are ordinary content and are followed; callers that must
// reject in-root symlinks too want MkdirAllNoFollow.
func (r *Root) MkdirAll(rel string, perm os.FileMode) error {
	return r.mutateWithRoot(rel, "creating directory %s", func(root rootHandle, clean string) error {
		return root.MkdirAll(clean, perm)
	})
}

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
			return fmt.Errorf("checking sync-tree ancestor %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: ancestor %s is not a directory", ErrUnsafePath, current)
		}
	}

	return nil
}

func (r *Root) MkdirAllNoFollow(rel string, perm os.FileMode) error {
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
		if err := ensureNoFollowDirectory(root, current, perm); err != nil {
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
// resolve outside the root fail closed. Callers that must also refuse in-root
// symlinks want RemoveTreeNoFollow.
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
		return fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	renameErr := r.ops.rename(root, srcClean, dstClean)
	closeErr := root.Close()
	if renameErr != nil {
		if closeErr != nil {
			return errors.Join(renameErr, closeErr)
		}

		srcPath := r.absForMessage(srcClean)

		return fmt.Errorf(
			"renaming %s to %s: %w",
			srcPath,
			r.absForMessage(dstClean),
			r.normalizeNotExist(srcPath, renameErr),
		)
	}

	if closeErr != nil {
		return fmt.Errorf("closing sync root %s: %w", r.dir, closeErr)
	}

	return nil
}

func (r *Root) SameFile(srcRel, dstRel string) (bool, error) {
	srcInfo, err := r.Stat(srcRel)
	if err != nil {
		return false, fmt.Errorf("stating %s: %w", srcRel, err)
	}
	dstInfo, err := r.Stat(dstRel)
	if err != nil {
		return false, fmt.Errorf("stating %s: %w", dstRel, err)
	}

	return os.SameFile(srcInfo, dstInfo), nil
}

func (r *Root) RenameWithTemporarySibling(srcRel, dstRel, tempStem string, attempts int) error {
	if attempts <= 0 {
		attempts = 1
	}
	if tempStem == "" {
		return fmt.Errorf("temporary rename stem is empty")
	}

	dstClean, err := cleanRelative(dstRel)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dstClean)
	tempRel := ""
	for i := 0; i < attempts; i++ {
		name := tempStem
		if i > 0 {
			name = fmt.Sprintf("%s-%d", tempStem, i)
		}
		candidate := filepath.Join(parent, name)
		if _, err := r.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			tempRel = candidate
			break
		} else if err != nil {
			return fmt.Errorf("checking temporary rename path %s: %w", candidate, err)
		}
	}
	if tempRel == "" {
		return fmt.Errorf("temporary rename path already exists under %s", parent)
	}

	if err := r.Rename(srcRel, tempRel); err != nil {
		return err
	}
	if err := r.Rename(tempRel, dstRel); err != nil {
		if rollbackErr := r.Rename(tempRel, srcRel); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rolling back temporary rename: %w", rollbackErr))
		}
		return err
	}

	return nil
}

// Chtimes sets access and modification times inside the sync root. See the
// osRootHandle.Chtimes note for the upstream Unix symlink-swap caveat.
func (r *Root) Chtimes(rel string, atime time.Time, mtime time.Time) error {
	return r.mutateWithRoot(rel, "setting times on %s", func(root rootHandle, clean string) error {
		return r.ops.chtimes(root, clean, atime, mtime)
	})
}

// WalkDir walks the sync tree and calls fn with absolute paths rooted under r.
func (r *Root) WalkDir(fn fs.WalkDirFunc) error {
	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}
	defer root.Close() //nolint:errcheck // rooted handle release; mutation errors are reported by the operation itself

	if err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		absPath := r.dir
		if rel != "." {
			absPath = filepath.Join(r.dir, filepath.FromSlash(rel))
		}

		return fn(absPath, d, walkErr)
	}); err != nil {
		return fmt.Errorf("walking sync tree %s: %w", r.dir, err)
	}

	return nil
}

func (r *Root) normalizeNotExist(path string, original error) error {
	if original == nil {
		return nil
	}

	// Only a positive ErrUnsafePath determination overrides the original
	// error. If the ancestor walk itself fails for an unrelated reason, the
	// caller's error is the more useful one and must not be masked.
	if path != r.dir {
		if rel, relErr := r.relativeFromAbs(path); relErr == nil {
			if ancestorErr := r.ValidateNoSymlinkAncestors(rel); errors.Is(ancestorErr, ErrUnsafePath) {
				return ancestorErr
			}
		}
	}

	if _, statErr := r.ops.lstatAbs(path); errors.Is(statErr, os.ErrNotExist) {
		return os.ErrNotExist
	}

	return original
}

func (r *Root) validateRootDirectoryNoFollow() error {
	info, err := r.ops.lstatAbs(r.dir)
	if err != nil {
		return fmt.Errorf("checking sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: root %s is not a directory", ErrUnsafePath, r.dir)
	}

	return nil
}

func (r *Root) openRootNoFollow() (rootHandle, error) {
	if err := r.validateRootDirectoryNoFollow(); err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	return root, nil
}

func ensureNoFollowDirectory(root rootHandle, rel string, perm os.FileMode) error {
	info, err := root.Lstat(rel)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: path %s is not a directory", ErrUnsafePath, rel)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking directory %s: %w", rel, err)
	}
	if err := root.Mkdir(rel, perm); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := root.Lstat(rel)
			if statErr != nil {
				return fmt.Errorf("checking directory %s after create race: %w", rel, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("%w: path %s is not a directory", ErrUnsafePath, rel)
			}
			return nil
		}
		return fmt.Errorf("creating directory %s: %w", rel, err)
	}

	return nil
}
