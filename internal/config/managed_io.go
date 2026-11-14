package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// configIO is the package's file-access seam: production uses
// defaultConfigIO, and tests substitute single functions to inject failures.
// The two writers are used by drive enrollment, whose steps a test fails one
// at a time.
type configIO struct {
	readManagedFile   func(path string) ([]byte, error)
	writeManagedFile  func(path string, data []byte) error
	removeManagedFile func(path string) error
	statManagedPath   func(path string) (os.FileInfo, error)
	readManagedDir    func(path string) ([]os.DirEntry, error)
	statLocalPath     func(path string) (os.FileInfo, error)
}

func defaultConfigIO() configIO {
	return configIO{
		readManagedFile:   readManagedFile,
		writeManagedFile:  atomicWriteFile,
		removeManagedFile: removeManagedFile,
		statManagedPath:   statManagedPath,
		readManagedDir:    readManagedDir,
		statLocalPath:     localpath.Stat,
	}
}

// readManagedFile reads one repo-managed config/data file by establishing the
// parent root capability once per call, then reading the relative file name.
// Config entrypoints still accept path strings, so OpenPath is the honest
// boundary constructor for those APIs.
func readManagedFile(path string) ([]byte, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("opening managed root for %s: %w", path, err)
	}

	data, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading managed file %s: %w", path, err)
	}

	return data, nil
}

// readManagedFileIfExists reads a managed file's bytes; a missing file is
// reported as absent, not as an error.
func readManagedFileIfExists(io configIO, path string) ([]byte, bool, error) {
	data, err := io.readManagedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	return data, true, nil
}

// removeManagedFile removes one managed file. Removing a file that is already
// gone is not an error.
func removeManagedFile(path string) error {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening managed root for %s: %w", path, err)
	}

	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing managed file %s: %w", path, err)
	}

	return nil
}

func statManagedPath(path string) (os.FileInfo, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("opening managed root for %s: %w", path, err)
	}

	info, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("stating managed file %s: %w", path, err)
	}

	return info, nil
}

func readManagedDir(path string) ([]os.DirEntry, error) {
	root, err := fsroot.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening managed directory %s: %w", path, err)
	}

	entries, err := root.ReadDir("")
	if err != nil {
		return nil, fmt.Errorf("reading managed directory %s: %w", path, err)
	}

	return entries, nil
}
