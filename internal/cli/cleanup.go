package cli

import (
	"errors"
	"fmt"
	"os"

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
// file, or a missing parent, is absent; fsroot counts a parent, or any folder
// above it, that is a file as missing, since nothing can be beneath a file.
// Every other failure to check is returned: reading permission denied, an I/O
// error or a symlink loop as "absent" made an unreadable data directory look
// like one with no login and nothing synced.
func managedPathExists(path string) (bool, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return false, fmt.Errorf("open managed path %s: %w", path, err)
	}

	_, err = root.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, fmt.Errorf("check %s: %w", path, err)
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
