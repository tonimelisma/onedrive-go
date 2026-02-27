package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

const platformDarwin = "darwin"

// defaultTrashFunc moves a file or directory to the OS trash.
// On macOS, items are moved to ~/.Trash/ (always available).
// On Linux, this returns an error — opt-in only via XDG trash (future).
func defaultTrashFunc(absPath string) error {
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
	if _, statErr := os.Stat(trashDir); statErr != nil {
		return fmt.Errorf("trash directory not found: %w", statErr)
	}

	name := filepath.Base(absPath)
	dest := filepath.Join(trashDir, name)

	// Handle name collisions: append " 2", " 3", etc. (matching Finder behavior).
	if _, statErr := os.Stat(dest); statErr == nil {
		stem := name
		ext := filepath.Ext(name)

		if ext != "" {
			stem = name[:len(name)-len(ext)]
		}

		for i := 2; ; i++ {
			candidate := filepath.Join(trashDir, stem+" "+strconv.Itoa(i)+ext)
			if _, statErr := os.Stat(candidate); os.IsNotExist(statErr) {
				dest = candidate
				break
			}
		}
	}

	return os.Rename(absPath, dest)
}
