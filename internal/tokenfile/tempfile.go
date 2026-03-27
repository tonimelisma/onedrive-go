package tokenfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"golang.org/x/oauth2"
)

func marshalTokenFile(tok *oauth2.Token) ([]byte, error) {
	if tok == nil {
		return nil, fmt.Errorf("tokenfile: refusing to save nil token")
	}

	tf := File{Token: tok}

	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("tokenfile: encoding: %w", err)
	}

	return data, nil
}

func setTempFilePermissions(file *os.File) error {
	if err := chmodTrustedTempPath(file.Name(), FilePerms); err != nil {
		return err
	}

	return nil
}

func writeTempFileData(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("tokenfile: writing: %w", err)
	}

	return nil
}

func syncTempFile(file *os.File) error {
	if err := file.Sync(); err != nil {
		return fmt.Errorf("tokenfile: syncing: %w", err)
	}

	return nil
}

func closeTempFile(file *os.File, prior error) error {
	closeErr := file.Close()
	if closeErr != nil {
		wrapped := fmt.Errorf("tokenfile: closing temp file: %w", closeErr)
		if prior == nil {
			return wrapped
		}

		return errors.Join(prior, wrapped)
	}

	return prior
}

func removeTempPath(path string, prior error) error {
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		wrapped := fmt.Errorf("tokenfile: removing temp file: %w", removeErr)
		if prior == nil {
			return wrapped
		}

		return errors.Join(prior, wrapped)
	}

	return prior
}

func renameTempFile(src, dst string) error {
	if err := renameTrustedTempPath(src, dst); err != nil {
		return err
	}

	return nil
}

func chmodTrustedTempPath(path string, mode os.FileMode) error {
	// Path is created by os.CreateTemp in the destination directory, so it is
	// an internally-derived atomic-write temp file rather than external input.
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // Path is an os.CreateTemp-managed temp file in the token directory.
		return fmt.Errorf("tokenfile: setting permissions: %w", err)
	}

	return nil
}

func renameTrustedTempPath(src, dst string) error {
	// Source temp path is internally-derived and the destination is the final
	// token-managed path in the same directory for an atomic rename.
	if err := os.Rename(src, dst); err != nil { //nolint:gosec // Source and destination are managed atomic-write paths in the token directory.
		return fmt.Errorf("tokenfile: renaming: %w", err)
	}

	return nil
}
