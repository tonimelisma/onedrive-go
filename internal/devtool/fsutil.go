package devtool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Everything devtool writes is private to the user who runs it: verify
// logs, summaries and benchmark results can quote environment details.
const (
	devtoolWriteFilePerm = 0o600
	devtoolWriteDirPerm  = 0o700
)

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return data, nil
}

func writeFile(path string, data []byte) error {
	return atomicWrite(path, data, ".devtool-write-*.tmp")
}

// statPath reports whether path can be stat'ed; the error wraps the cause,
// os.ErrNotExist among them.
func statPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	return nil
}

// mkdirAll makes path and its parents, private to the user.
func mkdirAll(path string) error {
	if err := os.MkdirAll(path, devtoolWriteDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}

	return nil
}

func mkdirTemp(dir string, pattern string) (string, error) {
	tempDir, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("mkdir temp in %s: %w", dir, err)
	}

	return tempDir, nil
}

func createTemp(dir string, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}

	return file, nil
}

func open(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return file, nil
}

func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return file, nil
}

func remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

func removeAll(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove all %s: %w", path, err)
	}

	return nil
}

// atomicWrite writes data to path through a temp file in its directory,
// synced and renamed into place, private to the user.
func atomicWrite(path string, data []byte, tempPattern string) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, devtoolWriteDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tempFile, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}

	tempPath := tempFile.Name()
	defer func() {
		if retErr == nil {
			return
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			retErr = errorsJoin(retErr, fmt.Errorf("remove temp file %s: %w", tempPath, removeErr))
		}
	}()

	if _, err := tempFile.Write(data); err != nil {
		return closeWithPrimaryError(tempFile, fmt.Errorf("write temp file %s: %w", tempPath, err))
	}
	if err := tempFile.Chmod(devtoolWriteFilePerm); err != nil {
		return closeWithPrimaryError(tempFile, fmt.Errorf("chmod temp file %s: %w", tempPath, err))
	}
	if err := tempFile.Sync(); err != nil {
		return closeWithPrimaryError(tempFile, fmt.Errorf("sync temp file %s: %w", tempPath, err))
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file %s -> %s: %w", tempPath, path, err)
	}

	return nil
}

func errorsJoin(left error, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return fmt.Errorf("%w; %w", left, right)
}

func closeWithPrimaryError(file *os.File, primaryErr error) error {
	if closeErr := file.Close(); closeErr != nil {
		return errorsJoin(primaryErr, fmt.Errorf("close %s: %w", file.Name(), closeErr))
	}

	return primaryErr
}
