package sync

import (
	"os"
	"strings"
	stdsync "sync"

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

// permissionCache is a thread-safe in-memory cache of folder path → canWrite.
// Built from sync_failures + API queries each pass. Not persisted.
// Accessed concurrently by the main sync goroutine (recheckPermissions,
// deniedPrefixes) and the drain goroutine (handle403 → set).
type permissionCache struct {
	mu    stdsync.RWMutex
	cache map[string]bool
}

func newPermissionCache() *permissionCache {
	return &permissionCache{cache: make(map[string]bool)}
}

// reset clears all cached entries. Called at the start of each sync pass
// to prevent stale entries from persisting when permissions change.
func (pc *permissionCache) reset() {
	if pc == nil {
		return
	}

	pc.mu.Lock()
	pc.cache = make(map[string]bool)
	pc.mu.Unlock()
}

func (pc *permissionCache) get(folderPath string) (canWrite bool, ok bool) {
	if pc == nil {
		return false, false
	}

	pc.mu.RLock()
	canWrite, ok = pc.cache[folderPath]
	pc.mu.RUnlock()

	return canWrite, ok
}

func (pc *permissionCache) set(folderPath string, canWrite bool) {
	if pc == nil {
		return
	}

	pc.mu.Lock()
	pc.cache[folderPath] = canWrite
	pc.mu.Unlock()
}

// deniedPrefixes returns all folder paths cached as read-only (canWrite == false).
func (pc *permissionCache) deniedPrefixes() []string {
	if pc == nil {
		return nil
	}

	pc.mu.RLock()
	defer pc.mu.RUnlock()

	var prefixes []string
	for path, canWrite := range pc.cache {
		if !canWrite {
			prefixes = append(prefixes, path)
		}
	}

	return prefixes
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
	f, err := os.Open(dir)
	if err != nil {
		return false
	}

	f.Close()

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
