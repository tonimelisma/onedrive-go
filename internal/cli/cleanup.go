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

func managedPathExists(path string) bool {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return false
	}

	_, err = root.Stat(name)

	return err == nil
}
