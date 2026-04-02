package config

import (
	"fmt"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type configIO struct {
	readManagedFile func(path string) ([]byte, error)
	statManagedPath func(path string) (os.FileInfo, error)
	readManagedDir  func(path string) ([]os.DirEntry, error)
	statLocalPath   func(path string) (os.FileInfo, error)
}

func defaultConfigIO() configIO {
	return configIO{
		readManagedFile: readManagedFile,
		statManagedPath: statManagedPath,
		readManagedDir:  readManagedDir,
		statLocalPath:   localpath.Stat,
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
