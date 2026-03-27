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
	// Temp file path is created by os.CreateTemp in the destination directory,
	// so permission tightening applies to an internally-derived atomic-write path.
	if err := os.Chmod(file.Name(), FilePerms); err != nil { //nolint:gosec // Trusted atomic-write temp path in the token directory.
		return fmt.Errorf("tokenfile: setting permissions: %w", err)
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
	// Source temp path is created by os.CreateTemp beside the final token file,
	// so rename stays within the managed token directory.
	if err := os.Rename(src, dst); err != nil { //nolint:gosec // Trusted atomic-write rename within the token directory.
		return fmt.Errorf("tokenfile: renaming: %w", err)
	}

	return nil
}
