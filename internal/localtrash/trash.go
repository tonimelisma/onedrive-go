// Package localtrash owns OS-local trash behavior for sync execution.
package localtrash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const platformDarwin = "darwin"

// Default moves a file or directory to the OS trash.
// On macOS, items are moved to ~/.Trash/.
// On Linux, this returns an error — opt-in only via XDG trash (future).
func Default(absPath string) error {
	if runtime.GOOS != platformDarwin {
		return fmt.Errorf("trash not available on %s", runtime.GOOS)
	}

	return moveToMacOSTrash(absPath)
}

// moveToMacOSTrash moves a file or directory to the current user's ~/.Trash/.
// Handles name collisions by appending a numeric suffix.
func moveToMacOSTrash(absPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	trashDir := filepath.Join(home, ".Trash")

	// Verify the trash directory exists (it always does on macOS, but be safe).
	if _, statErr := localpath.Stat(trashDir); statErr != nil {
		return fmt.Errorf("trash directory not found: %w", statErr)
	}

	name := filepath.Base(absPath)
	dest := filepath.Join(trashDir, name)

	// Handle name collisions: append " 2", " 3", etc. (matching Finder behavior).
	if _, statErr := localpath.Stat(dest); statErr == nil {
		stem := name
		ext := filepath.Ext(name)

		if ext != "" {
			stem = name[:len(name)-len(ext)]
		}

		for i := 2; ; i++ {
			candidate := filepath.Join(trashDir, stem+" "+strconv.Itoa(i)+ext)
			if _, statErr := localpath.Stat(candidate); errors.Is(statErr, os.ErrNotExist) {
				dest = candidate
				break
			}
		}
	}

	if err := localpath.Rename(absPath, dest); err != nil {
		return fmt.Errorf("move item to local trash: %w", err)
	}

	return nil
}
