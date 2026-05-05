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

type rootHandle interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	Mkdir(name string, perm os.FileMode) error
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

func (h *osRootHandle) FS() fs.FS {
	return h.root.FS()
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

type rootOps struct {
	openRoot  func(dir string) (rootHandle, error)
	mkdirAll  func(path string, perm os.FileMode) error
	remove    func(path string) error
	removeAll func(path string) error
	rename    func(oldpath, newpath string) error
	chtimes   func(path string, atime time.Time, mtime time.Time) error
	lstat     func(path string) (os.FileInfo, error)
	glob      func(pattern string) ([]string, error)
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
		mkdirAll:  os.MkdirAll,
		remove:    os.Remove,
		removeAll: os.RemoveAll,
		rename:    os.Rename,
		chtimes:   os.Chtimes,
		lstat:     os.Lstat,
		glob:      filepath.Glob,
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

func (r *Root) LstatAbs(abs string) (os.FileInfo, error) {
	rel, err := r.Rel(abs)
	if err != nil {
		return nil, err
	}

	return r.Lstat(rel)
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
	defer file.Close()

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

func (r *Root) MkdirAll(rel string, perm os.FileMode) error {
	path, err := r.Abs(rel)
	if err != nil {
		return err
	}

	if err := r.ops.mkdirAll(path, perm); err != nil {
		return fmt.Errorf("creating directory %s: %w", path, err)
	}

	return nil
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
	defer root.Close()

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
	defer root.Close()

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

func (r *Root) Remove(rel string) error {
	path, err := r.Abs(rel)
	if err != nil {
		return err
	}

	if err := r.ops.remove(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}

// RemoveEmptyDirNoFollow removes rel only if it is an empty directory within
// the rooted sync tree. The explicit empty check gives callers a clear contract;
// the final rmdir-style Remove remains the race guard if a child appears after
// the check.
func (r *Root) RemoveEmptyDirNoFollow(rel string) error {
	empty, err := r.DirEmptyNoFollow(rel)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("removing empty directory %s: directory is not empty", rel)
	}

	path, err := r.Abs(rel)
	if err != nil {
		return err
	}
	if err := r.ops.remove(path); err != nil {
		return fmt.Errorf("removing empty directory %s: %w", rel, err)
	}

	return nil
}

func (r *Root) RemoveAll(rel string) error {
	path, err := r.Abs(rel)
	if err != nil {
		return err
	}

	if err := r.ops.removeAll(path); err != nil {
		return fmt.Errorf("removing tree %s: %w", path, err)
	}

	return nil
}

func (r *Root) RemoveAbs(abs string) error {
	rel, err := r.Rel(abs)
	if err != nil {
		return err
	}

	return r.Remove(rel)
}

func (r *Root) Rename(srcRel, dstRel string) error {
	srcPath, err := r.Abs(srcRel)
	if err != nil {
		return err
	}
	dstPath, err := r.Abs(dstRel)
	if err != nil {
		return err
	}

	if err := r.ops.rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", srcPath, dstPath, err)
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

func (r *Root) Chtimes(rel string, atime time.Time, mtime time.Time) error {
	path, err := r.Abs(rel)
	if err != nil {
		return err
	}

	if err := r.ops.chtimes(path, atime, mtime); err != nil {
		return fmt.Errorf("setting times on %s: %w", path, err)
	}

	return nil
}

// WalkDir walks the sync tree and calls fn with absolute paths rooted under r.
func (r *Root) WalkDir(fn fs.WalkDirFunc) error {
	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}
	defer root.Close()

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

// Glob matches a relative glob pattern within the sync root and returns rooted
// relative match paths.
func (r *Root) Glob(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is empty")
	}

	dirPattern := filepath.Dir(pattern)
	basePattern := filepath.Base(pattern)

	dirPath, err := r.Abs(dirPattern)
	if err != nil {
		return nil, err
	}

	matches, err := r.ops.glob(filepath.Join(dirPath, basePattern))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", filepath.Join(dirPath, basePattern), err)
	}

	relMatches := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, relErr := r.Rel(match)
		if relErr != nil {
			return nil, relErr
		}
		relMatches = append(relMatches, rel)
	}

	return relMatches, nil
}

func (r *Root) normalizeNotExist(path string, original error) error {
	if original == nil {
		return nil
	}

	if _, statErr := r.ops.lstat(path); errors.Is(statErr, os.ErrNotExist) {
		return os.ErrNotExist
	}

	return original
}

func (r *Root) validateRootDirectoryNoFollow() error {
	info, err := r.ops.lstat(r.dir)
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
