package config

import (
	"errors"
	"fmt"
	"os"
)

func removeTempPath(path, desc string, prior error) error {
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		wrapped := fmt.Errorf("removing %s: %w", desc, removeErr)
		if prior == nil {
			return wrapped
		}

		return errors.Join(prior, wrapped)
	}

	return prior
}

func closeTempFile(file *os.File, desc string, prior error) error {
	closeErr := file.Close()
	if closeErr != nil {
		wrapped := fmt.Errorf("closing %s: %w", desc, closeErr)
		if prior == nil {
			return wrapped
		}

		return errors.Join(prior, wrapped)
	}

	return prior
}

func chmodTrustedTempPath(path string, mode os.FileMode, desc string) error {
	if err := chmodManagedTempPath(path, mode); err != nil {
		return fmt.Errorf("setting %s permissions: %w", desc, err)
	}

	return nil
}

func renameTrustedTempPath(src, dst, desc string) error {
	if err := renameManagedTempPath(src, dst); err != nil {
		return fmt.Errorf("renaming %s: %w", desc, err)
	}

	return nil
}
