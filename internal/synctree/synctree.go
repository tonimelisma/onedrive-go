// Package synctree provides a rooted filesystem capability for sync-runtime
// operations under one validated sync root. Unlike localpath, callers do not
// re-establish trust on every call; unlike fsroot, this boundary models user
// content under the sync tree rather than repo-managed state files.
package synctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type rootHandle interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
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

func (h *osRootHandle) FS() fs.FS {
	return h.root.FS()
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

type rootOps struct {
	openRoot func(dir string) (rootHandle, error)
	mkdirAll func(path string, perm os.FileMode) error
	remove   func(path string) error
	rename   func(oldpath, newpath string) error
	lstat    func(path string) (os.FileInfo, error)
	glob     func(pattern string) ([]string, error)
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
		mkdirAll: os.MkdirAll,
		remove:   os.Remove,
		rename:   os.Rename,
		lstat:    os.Lstat,
		glob:     filepath.Glob,
	}
}

// Root is a rooted capability for sync-runtime filesystem operations.
type Root struct {
	dir string
	ops rootOps
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
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening sync root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	info, statErr := root.Stat(clean)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		target := r.dir
		if clean != "." {
			target = filepath.Join(r.dir, clean)
		}

		return nil, fmt.Errorf("stating %s: %w", target, r.normalizeNotExist(target, statErr))
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
