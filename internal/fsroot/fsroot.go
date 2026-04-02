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

// Root is a capability for managed files rooted under one trusted directory.
// Callers establish trust once by constructing the root, then operate on
// relative names within it. This keeps managed-state I/O explicit and avoids
// repeating ad hoc path handling at every call site.
type Root struct {
	dir string
}

// Open constructs a managed root for dir. The directory does not need to
// exist yet; callers may create it later via MkdirAll or AtomicWrite.
func Open(dir string) (*Root, error) {
	if dir == "" {
		return nil, fmt.Errorf("root directory is empty")
	}

	return &Root{dir: filepath.Clean(dir)}, nil
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
	opener func(root *os.Root, clean string) (*os.File, error),
) (*os.File, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, normalizeNotExist(filepath.Join(r.dir, clean), err))
	}

	file, openErr := opener(root, clean)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		return nil, normalizeNotExist(filepath.Join(r.dir, clean), openErr)
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
	file, err := r.openWithRoot(name, func(root *os.Root, clean string) (*os.File, error) {
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
	file, err := r.openWithRoot(name, func(root *os.Root, clean string) (*os.File, error) {
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

	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, normalizeNotExist(r.dir, err))
	}

	info, statErr := root.Stat(clean)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stating %s: %w", clean, normalizeNotExist(filepath.Join(r.dir, clean), statErr))
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

	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", r.dir, normalizeNotExist(r.dir, err))
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

		return nil, fmt.Errorf("reading directory %s: %w", cleanDir, normalizeNotExist(dirPath, readErr))
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing root %s: %w", r.dir, closeErr)
	}

	return entries, nil
}

func (r *Root) MkdirAll(perm os.FileMode) error {
	if err := os.MkdirAll(r.dir, perm); err != nil {
		return fmt.Errorf("creating root directory %s: %w", r.dir, err)
	}

	return nil
}

func (r *Root) Remove(name string) error {
	path, err := r.pathFor(name)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil {
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

	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}

	return nil
}

func (r *Root) CreateTemp(dirName, pattern string) (*os.File, string, error) {
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

	temp, err := os.CreateTemp(tempDir, pattern)
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
	if mkErr := os.MkdirAll(dirPath, dirPerm); mkErr != nil {
		return fmt.Errorf("creating target directory %s: %w", dirPath, mkErr)
	}

	temp, tempName, err := r.CreateTemp(targetDir, pattern)
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

func writeTempFile(temp *os.File, data []byte, filePerm os.FileMode) error {
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

func normalizeNotExist(path string, original error) error {
	if original == nil {
		return nil
	}

	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		return os.ErrNotExist
	}

	return original
}

func closeWithContext(temp *os.File, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
