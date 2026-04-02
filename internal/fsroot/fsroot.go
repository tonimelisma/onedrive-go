// Package fsroot provides root-bound capabilities for managed state files.
// Callers establish a trusted directory once, then operate on relative names
// within that root so atomic writes and path validation stay explicit.
package fsroot

import (
	"errors"
	"fmt"
	"io"
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
	//nolint:wrapcheck // openWithRoot adds the managed-root boundary context.
	return h.root.Open(name)
}

func (h *osRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	//nolint:wrapcheck // openWithRoot adds the managed-root boundary context.
	return h.root.OpenFile(name, flag, perm)
}

func (h *osRootHandle) Stat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // caller adds path-specific context after containment checks.
	return h.root.Stat(name)
}

func (h *osRootHandle) FS() fs.FS {
	return h.root.FS()
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

type tempFile interface {
	Name() string
	Chmod(perm os.FileMode) error
	Write(p []byte) (n int, err error)
	Sync() error
	Close() error
}

type rootOps struct {
	openRoot   func(dir string) (rootHandle, error)
	mkdirAll   func(path string, perm os.FileMode) error
	remove     func(path string) error
	rename     func(oldpath, newpath string) error
	createTemp func(dir, pattern string) (tempFile, error)
	lstat      func(path string) (os.FileInfo, error)
}

func defaultRootOps() rootOps {
	return rootOps{
		openRoot: func(dir string) (rootHandle, error) {
			root, err := os.OpenRoot(dir)
			if err != nil {
				//nolint:wrapcheck // callers add the concrete managed-root path context.
				return nil, err
			}

			return &osRootHandle{root: root}, nil
		},
		mkdirAll: os.MkdirAll,
		remove:   os.Remove,
		rename:   os.Rename,
		createTemp: func(dir, pattern string) (tempFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		lstat: os.Lstat,
	}
}

// Root is a capability for managed files rooted under one trusted directory.
// Callers establish trust once by constructing the root, then operate on
// relative names within it. This keeps managed-state I/O explicit and avoids
// repeating ad hoc path handling at every call site.
type Root struct {
	dir string
	ops rootOps
}

// Open constructs a managed root for dir. The directory does not need to
// exist yet; callers may create it later via MkdirAll or AtomicWrite.
func Open(dir string) (*Root, error) {
	if dir == "" {
		return nil, fmt.Errorf("root directory is empty")
	}

	return &Root{
		dir: filepath.Clean(dir),
		ops: defaultRootOps(),
	}, nil
}

// OpenPath splits a managed file path into its parent root capability plus the
// relative name within that root.
func OpenPath(path string) (*Root, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("path is empty")
	}
	if path != string(filepath.Separator) && os.IsPathSeparator(path[len(path)-1]) {
		return nil, "", fmt.Errorf("path %q does not name a file", path)
	}

	root, err := Open(filepath.Dir(path))
	if err != nil {
		return nil, "", err
	}

	name, err := cleanRelative(filepath.Base(path))
	if err != nil {
		return nil, "", err
	}

	return root, name, nil
}

func cleanRelative(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("relative path is empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("relative path %q must not be absolute", name)
	}

	clean := filepath.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("relative path %q does not name a file", name)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative path %q escapes root", name)
	}

	return clean, nil
}

func (r *Root) pathFor(name string) (string, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return "", err
	}

	return filepath.Join(r.dir, clean), nil
}

func (r *Root) openWithRoot(
	name string,
	opener func(root rootHandle, clean string) (*os.File, error),
) (*os.File, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, r.normalizeNotExist(filepath.Join(r.dir, clean), err))
	}

	file, openErr := opener(root, clean)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		return nil, r.normalizeNotExist(filepath.Join(r.dir, clean), openErr)
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, errors.Join(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing root %s: %w", r.dir, closeErr)
	}

	return file, nil
}

func (r *Root) Open(name string) (*os.File, error) {
	file, err := r.openWithRoot(name, func(root rootHandle, clean string) (*os.File, error) {
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

func (r *Root) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := r.openWithRoot(name, func(root rootHandle, clean string) (*os.File, error) {
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

func (r *Root) ReadFile(name string) ([]byte, error) {
	file, err := r.Open(name)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading %s: %w", name, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", name, closeErr)
	}

	return data, nil
}

func (r *Root) Stat(name string) (os.FileInfo, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return nil, err
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	info, statErr := root.Stat(clean)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stating %s: %w", clean, r.normalizeNotExist(filepath.Join(r.dir, clean), statErr))
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing root %s: %w", r.dir, closeErr)
	}

	return info, nil
}

func (r *Root) ReadDir(dirName string) ([]os.DirEntry, error) {
	cleanDir := "."
	if dirName != "" && filepath.Clean(dirName) != "." {
		var err error
		cleanDir, err = cleanRelative(dirName)
		if err != nil {
			return nil, err
		}
	}

	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, r.normalizeNotExist(r.dir, err))
	}

	entries, readErr := fs.ReadDir(root.FS(), cleanDir)
	closeErr := root.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		dirPath := r.dir
		if cleanDir != "." {
			dirPath = filepath.Join(r.dir, cleanDir)
		}

		return nil, fmt.Errorf("reading directory %s: %w", cleanDir, r.normalizeNotExist(dirPath, readErr))
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing root %s: %w", r.dir, closeErr)
	}

	return entries, nil
}

func (r *Root) MkdirAll(perm os.FileMode) error {
	if err := r.ops.mkdirAll(r.dir, perm); err != nil {
		return fmt.Errorf("creating root directory %s: %w", r.dir, err)
	}

	return nil
}

func (r *Root) Remove(name string) error {
	path, err := r.pathFor(name)
	if err != nil {
		return err
	}

	if err := r.ops.remove(path); err != nil {
		return fmt.Errorf("removing %s: %w", name, err)
	}

	return nil
}

func (r *Root) Rename(src, dst string) error {
	srcPath, err := r.pathFor(src)
	if err != nil {
		return err
	}
	dstPath, err := r.pathFor(dst)
	if err != nil {
		return err
	}

	if err := r.ops.rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}

	return nil
}

func (r *Root) CreateTemp(dirName, pattern string) (*os.File, string, error) {
	temp, name, err := r.createTempFile(dirName, pattern)
	if err != nil {
		return nil, "", err
	}

	osTemp, ok := temp.(*os.File)
	if !ok {
		closeErr := temp.Close()
		if closeErr != nil {
			return nil, "", errors.Join(fmt.Errorf("creating temp file for %s: temp handle is not *os.File", name), closeErr)
		}

		return nil, "", fmt.Errorf("creating temp file for %s: temp handle is not *os.File", name)
	}

	return osTemp, name, nil
}

func (r *Root) createTempFile(dirName, pattern string) (tempFile, string, error) {
	cleanDir := "."
	if dirName != "" && filepath.Clean(dirName) != "." {
		var err error
		cleanDir, err = cleanRelative(dirName)
		if err != nil {
			return nil, "", err
		}
	}

	tempDir := r.dir
	if cleanDir != "." {
		tempDir = filepath.Join(r.dir, cleanDir)
	}

	temp, err := r.ops.createTemp(tempDir, pattern)
	if err != nil {
		return nil, "", fmt.Errorf("creating temp file in %s: %w", tempDir, err)
	}

	name := filepath.Base(temp.Name())
	if cleanDir != "." {
		name = filepath.Join(cleanDir, name)
	}

	return temp, name, nil
}

func (r *Root) AtomicWrite(
	name string,
	data []byte,
	filePerm os.FileMode,
	dirPerm os.FileMode,
	pattern string,
) (err error) {
	cleanName, err := cleanRelative(name)
	if err != nil {
		return err
	}

	targetDir, dirPath := r.targetDir(cleanName)
	if mkErr := r.ops.mkdirAll(dirPath, dirPerm); mkErr != nil {
		return fmt.Errorf("creating target directory %s: %w", dirPath, mkErr)
	}

	temp, tempName, err := r.createTempFile(targetDir, pattern)
	if err != nil {
		return err
	}

	defer func() { err = r.cleanupTempOnError(tempName, err) }()

	if writeErr := writeTempFile(temp, data, filePerm); writeErr != nil {
		return writeErr
	}

	if renameErr := r.Rename(tempName, cleanName); renameErr != nil {
		return renameErr
	}

	err = nil

	return nil
}

func (r *Root) targetDir(cleanName string) (string, string) {
	targetDir := filepath.Dir(cleanName)
	dirPath := r.dir
	if targetDir != "." {
		dirPath = filepath.Join(r.dir, targetDir)
	}

	return targetDir, dirPath
}

func (r *Root) cleanupTempOnError(tempName string, currentErr error) error {
	if currentErr == nil {
		return nil
	}

	removeErr := r.Remove(tempName)
	if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
		return currentErr
	}

	return errors.Join(currentErr, removeErr)
}

func writeTempFile(temp tempFile, data []byte, filePerm os.FileMode) error {
	if chmodErr := temp.Chmod(filePerm); chmodErr != nil {
		return closeWithContext(temp, "setting temp file permissions", chmodErr)
	}

	if _, writeErr := temp.Write(data); writeErr != nil {
		return closeWithContext(temp, "writing temp file", writeErr)
	}

	if syncErr := temp.Sync(); syncErr != nil {
		return closeWithContext(temp, "syncing temp file", syncErr)
	}

	if closeErr := temp.Close(); closeErr != nil {
		return fmt.Errorf("closing temp file: %w", closeErr)
	}

	return nil
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

func closeWithContext(temp tempFile, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
