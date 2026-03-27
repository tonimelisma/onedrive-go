// Package trustedpath provides root-based file operations for already-vetted
// absolute or workspace-relative paths. It narrows file access to the final
// path component under an opened directory root so callers do not need
// repeated inline gosec exceptions for managed paths.
package trustedpath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func split(path string) (string, string, error) {
	if path == "" {
		return "", "", fmt.Errorf("path is empty")
	}
	if path != string(filepath.Separator) && os.IsPathSeparator(path[len(path)-1]) {
		return "", "", fmt.Errorf("path %q does not name a file", path)
	}

	dir := filepath.Dir(path)
	leaf := filepath.Base(path)
	if leaf == "." || leaf == string(filepath.Separator) {
		return "", "", fmt.Errorf("path %q does not name a file", path)
	}

	return dir, leaf, nil
}

func openWithRoot(path string, opener func(*os.Root, string) (*os.File, error)) (*os.File, error) {
	dir, leaf, err := split(path)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening root %s: %w", dir, err)
	}

	file, openErr := opener(root, leaf)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		return nil, openErr
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, errors.Join(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing root %s: %w", dir, closeErr)
	}

	return file, nil
}

func Open(path string) (*os.File, error) {
	file, err := openWithRoot(path, func(root *os.Root, leaf string) (*os.File, error) {
		f, openErr := root.Open(leaf)
		if openErr != nil {
			return nil, fmt.Errorf("opening %s: %w", path, openErr)
		}

		return f, nil
	})
	if err != nil {
		return nil, err
	}

	return file, nil
}

func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := openWithRoot(path, func(root *os.Root, leaf string) (*os.File, error) {
		f, openErr := root.OpenFile(leaf, flag, perm)
		if openErr != nil {
			return nil, fmt.Errorf("opening %s: %w", path, openErr)
		}

		return f, nil
	})
	if err != nil {
		return nil, err
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
