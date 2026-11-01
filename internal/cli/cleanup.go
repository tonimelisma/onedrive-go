package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

func removePathIfExists(path string) error {
	_, err := removeManagedPath(path)
	return err
}

func removeManagedPath(path string) (bool, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return false, fmt.Errorf("open managed path %s: %w", path, err)
	}

	err = root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove %s: %w", path, err)
	}

	return true, nil
}

// managedPathExists reports whether a managed file is on disk. Only a missing
// file, or a missing parent (including a parent that is not a directory), is
// absent. Every other failure to check is returned: reading permission denied,
// an I/O error or a symlink loop as "absent" made an unreadable data directory
// look like one with no login and nothing synced.
func managedPathExists(path string) (bool, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return false, fmt.Errorf("open managed path %s: %w", path, err)
	}

	_, err = root.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) || managedParentIsNotADirectory(path) {
		return false, nil
	}

	return false, fmt.Errorf("check %s: %w", path, err)
}

// managedParentIsNotADirectory reports a managed path whose parent exists but
// is not a directory, under which no file can exist. os.Root reports opening
// such a parent with a plain "not a directory" error that is neither
// fs.ErrNotExist nor syscall.ENOTDIR, so it is recognized by looking at the
// parent itself.
func managedParentIsNotADirectory(path string) bool {
	parentRoot, parentName, err := fsroot.OpenPath(filepath.Dir(path))
	if err != nil {
		return false
	}

	info, err := parentRoot.Stat(parentName)

	return err == nil && !info.IsDir()
}

func readManagedFileIfExists(path string) ([]byte, bool, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, false, fmt.Errorf("open managed path %s: %w", path, err)
	}

	data, err := root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	return data, true, nil
}

func writeManagedFile(path string, data []byte, filePerm os.FileMode, dirPerm os.FileMode, tempPattern string) error {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("open managed path %s: %w", path, err)
	}

	if err := root.AtomicWrite(name, data, filePerm, dirPerm, tempPattern); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
