// Package localpath provides explicit arbitrary-local-path filesystem
// operations. Unlike fsroot, it does not model a pre-established managed root;
// each call treats the supplied local path itself as the trust boundary.
package localpath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func cleanPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}

	clean := filepath.Clean(path)
	if clean == "" {
		return "", fmt.Errorf("path is empty")
	}

	return clean, nil
}

func absolutePath(path string) (string, error) {
	clean, err := cleanPath(path)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("resolving path %s: %w", clean, err)
	}

	return abs, nil
}

func Open(path string) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	return file, nil
}

func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.OpenFile(abs, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	return file, nil
}

func ReadFile(path string) ([]byte, error) {
	file, err := Open(path)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading %s: %w", path, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", path, closeErr)
	}

	return data, nil
}

func Stat(path string) (os.FileInfo, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	// #nosec G703 -- localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stating %s: %w", path, err)
	}

	return info, nil
}

func Lstat(path string) (os.FileInfo, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	// #nosec G703 -- localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("lstating %s: %w", path, err)
	}

	return info, nil
}

func MkdirAll(path string, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(abs, perm); err != nil {
		return fmt.Errorf("creating directory %s: %w", path, err)
	}

	return nil
}

func Remove(path string) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}

func Rename(src, dst string) error {
	srcPath, err := absolutePath(src)
	if err != nil {
		return err
	}
	dstPath, err := absolutePath(dst)
	if err != nil {
		return err
	}

	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
	}

	return nil
}

func ReadDir(path string) ([]os.DirEntry, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", path, err)
	}

	return entries, nil
}

func EvalSymlinks(path string) (string, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving symlinks for %s: %w", path, err)
	}

	return resolved, nil
}

func Chtimes(path string, atime, mtime time.Time) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Chtimes(abs, atime, mtime); err != nil {
		return fmt.Errorf("setting times on %s: %w", path, err)
	}

	return nil
}

func CreateTemp(dir, pattern string) (*os.File, error) {
	abs, err := absolutePath(dir)
	if err != nil {
		return nil, err
	}

	file, err := os.CreateTemp(abs, pattern)
	if err != nil {
		return nil, fmt.Errorf("creating temp file in %s: %w", dir, err)
	}

	return file, nil
}

func RemoveAll(path string) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("removing tree %s: %w", path, err)
	}

	return nil
}

func Symlink(src, dst string) error {
	srcPath, err := absolutePath(src)
	if err != nil {
		return err
	}
	dstPath, err := absolutePath(dst)
	if err != nil {
		return err
	}

	if err := os.Symlink(srcPath, dstPath); err != nil {
		return fmt.Errorf("symlinking %s to %s: %w", src, dst, err)
	}

	return nil
}

func WriteFile(path string, data []byte, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.WriteFile(abs, data, perm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// AtomicWrite writes data to path via a temp file created in the target
// directory, then fsyncs, closes, and renames it into place.
func AtomicWrite(path string, data []byte, filePerm, dirPerm os.FileMode, pattern string) (err error) {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	targetDir := filepath.Dir(abs)
	if mkErr := os.MkdirAll(targetDir, dirPerm); mkErr != nil {
		return fmt.Errorf("creating target directory %s: %w", targetDir, mkErr)
	}

	temp, err := os.CreateTemp(targetDir, pattern)
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", targetDir, err)
	}

	tempPath := temp.Name()
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temp file %s: %w", tempPath, removeErr))
		}
	}()

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

	// #nosec G703 -- tempPath is created in targetDir and abs is normalized by absolutePath.
	if renameErr := os.Rename(tempPath, abs); renameErr != nil {
		return fmt.Errorf("renaming temp file into %s: %w", path, renameErr)
	}

	return nil
}

func closeWithContext(temp *os.File, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
