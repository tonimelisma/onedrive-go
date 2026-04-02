// Package localpath provides explicit arbitrary-local-path filesystem
// operations. Unlike fsroot, it does not model a pre-established managed root;
// each call treats the supplied local path itself as the trust boundary.
package localpath

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

func rootName(path string) (string, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return "", err
	}

	name := strings.TrimPrefix(abs, string(filepath.Separator))
	if name == "" {
		return ".", nil
	}

	return name, nil
}

func openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, fmt.Errorf("opening local root: %w", err)
	}

	return root, nil
}

func Open(path string) (*os.File, error) {
	name, err := rootName(path)
	if err != nil {
		return nil, err
	}

	root, err := openRoot()
	if err != nil {
		return nil, err
	}

	file, openErr := root.Open(name)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		return nil, fmt.Errorf("opening %s: %w", path, openErr)
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, errors.Join(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing local root: %w", closeErr)
	}

	return file, nil
}

func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	name, err := rootName(path)
	if err != nil {
		return nil, err
	}

	root, err := openRoot()
	if err != nil {
		return nil, err
	}

	file, openErr := root.OpenFile(name, flag, perm)
	closeErr := root.Close()
	if openErr != nil {
		if closeErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}

		return nil, fmt.Errorf("opening %s: %w", path, openErr)
	}

	if closeErr != nil {
		if fileCloseErr := file.Close(); fileCloseErr != nil {
			return nil, errors.Join(closeErr, fileCloseErr)
		}

		return nil, fmt.Errorf("closing local root: %w", closeErr)
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
	name, err := rootName(path)
	if err != nil {
		return nil, err
	}

	root, err := openRoot()
	if err != nil {
		return nil, err
	}

	info, statErr := root.Stat(name)
	closeErr := root.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stating %s: %w", path, statErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing local root: %w", closeErr)
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
	name, err := rootName(path)
	if err != nil {
		return err
	}

	root, err := openRoot()
	if err != nil {
		return err
	}

	removeErr := root.Remove(name)
	closeErr := root.Close()
	if removeErr != nil {
		if closeErr != nil {
			return errors.Join(removeErr, closeErr)
		}

		return fmt.Errorf("removing %s: %w", path, removeErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing local root: %w", closeErr)
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
	name, err := rootName(path)
	if err != nil {
		return nil, err
	}

	root, err := openRoot()
	if err != nil {
		return nil, err
	}

	entries, readErr := fs.ReadDir(root.FS(), name)
	closeErr := root.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading directory %s: %w", path, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing local root: %w", closeErr)
	}

	return entries, nil
}
