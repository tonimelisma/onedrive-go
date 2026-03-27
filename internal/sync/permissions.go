package sync

import (
	"os"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// resolveRemoteItemID looks up the remote item ID for a local path from the
// baseline. Pure data lookup — no store access needed.
func resolveRemoteItemID(bl *synctypes.Baseline, localPath string, driveID driveid.ID) string {
	entry, ok := bl.GetByPath(localPath)
	if !ok {
		return ""
	}

	if entry.DriveID != driveID {
		return ""
	}

	return entry.ItemID
}

// findShortcutForPath returns the first shortcut whose LocalPath is a prefix
// of (or equal to) the given path. Returns nil if no shortcut matches.
func findShortcutForPath(shortcuts []synctypes.Shortcut, filePath string) *synctypes.Shortcut {
	for i := range shortcuts {
		sc := &shortcuts[i]
		if filePath == sc.LocalPath || strings.HasPrefix(filePath, sc.LocalPath+"/") {
			return sc
		}
	}

	return nil
}

// isDirAccessible returns true if the directory can be opened for reading.
// os.Stat is insufficient — it succeeds on chmod 000 dirs because stat()
// only requires execute on the parent. os.Open tests actual read access.
func isDirAccessible(dir string) bool {
	f, err := os.Open(dir) //nolint:gosec // Accessibility probe operates on the caller-selected local sync directory.
	if err != nil {
		return false
	}

	if closeErr := f.Close(); closeErr != nil {
		return false
	}

	return true
}

// pathSetFromEvents builds a set of paths from scanner change events.
func pathSetFromEvents(events []synctypes.ChangeEvent) map[string]bool {
	if len(events) == 0 {
		return nil
	}

	paths := make(map[string]bool, len(events))
	for i := range events {
		if events[i].Path != "" {
			paths[events[i].Path] = true
		}
	}

	return paths
}

// pathSetFromBatch builds a set of paths from watch-mode batch entries.
func pathSetFromBatch(batch []synctypes.PathChanges) map[string]bool {
	if len(batch) == 0 {
		return nil
	}

	paths := make(map[string]bool, len(batch))
	for i := range batch {
		if batch[i].Path != "" {
			paths[batch[i].Path] = true
		}
	}

	return paths
}
