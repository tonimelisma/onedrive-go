package config

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

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
