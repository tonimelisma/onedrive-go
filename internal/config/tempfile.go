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
	// Path is created by os.CreateTemp in the destination directory, so it is
	// an internally-derived atomic-write temp file rather than external input.
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // Trusted atomic-write temp path in the destination directory.
		return fmt.Errorf("setting %s permissions: %w", desc, err)
	}

	return nil
}

func renameTrustedTempPath(src, dst, desc string) error {
	// Source temp path is internally-derived and the destination is the final
	// config-managed path in the same directory for an atomic rename.
	if err := os.Rename(src, dst); err != nil { //nolint:gosec // Trusted atomic-write rename within the config directory.
		return fmt.Errorf("renaming %s: %w", desc, err)
	}

	return nil
}
