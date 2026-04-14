package sync

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	// POSIX access(2) mode bits. Go's syscall package exposes Access on Unix
	// platforms but does not export the named R_OK/W_OK/X_OK constants.
	accessModeExecute uint32 = 0x1
	accessModeWrite   uint32 = 0x2
	accessModeRead    uint32 = 0x4
)

// resolveRemoteItemID looks up the remote item ID for a local path from the
// baseline. Pure data lookup — no store access needed.
func resolveRemoteItemID(bl *Baseline, localPath string, driveID driveid.ID) string {
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
func findShortcutForPath(shortcuts []Shortcut, filePath string) *Shortcut {
	for i := range shortcuts {
		sc := &shortcuts[i]
		if sc.LocalPath == "" {
			return sc
		}
		if filePath == sc.LocalPath || strings.HasPrefix(filePath, sc.LocalPath+"/") {
			return sc
		}
	}

	return nil
}

// isDirAccessible returns true if the directory can be opened for reading.
// os.Stat is insufficient — it succeeds on chmod 000 dirs because stat()
// only requires execute on the parent. os.Open tests actual read access.
func isDirAccessible(tree *synctree.Root, dir string) bool {
	var (
		f   *os.File
		err error
	)
	if filepath.IsAbs(dir) {
		f, err = tree.OpenAbs(dir)
	} else {
		f, err = tree.Open(dir)
	}
	if err != nil {
		return false
	}

	if closeErr := f.Close(); closeErr != nil {
		return false
	}

	return true
}

func dirAccessModeForCapability(capability PermissionCapability) (uint32, bool) {
	switch capability {
	case PermissionCapabilityLocalRead:
		return accessModeRead | accessModeExecute, true
	case PermissionCapabilityLocalWrite:
		return accessModeWrite | accessModeExecute, true
	case PermissionCapabilityUnknown, PermissionCapabilityRemoteRead, PermissionCapabilityRemoteWrite:
		return 0, false
	default:
		return 0, false
	}
}

func isDirAccessibleForCapability(tree *synctree.Root, dir string, capability PermissionCapability) bool {
	mode, ok := dirAccessModeForCapability(capability)
	if !ok {
		return false
	}

	absPath := dir
	if !filepath.IsAbs(absPath) {
		var err error
		absPath, err = tree.Abs(dir)
		if err != nil {
			return false
		}
	}

	return syscall.Access(absPath, mode) == nil
}

func isFileReadable(tree *synctree.Root, path string) bool {
	var (
		f   *os.File
		err error
	)
	if filepath.IsAbs(path) {
		f, err = tree.OpenAbs(path)
	} else {
		f, err = tree.Open(path)
	}
	if err != nil {
		return false
	}

	return f.Close() == nil
}

func isFileWritable(tree *synctree.Root, path string) bool {
	absPath := path
	if !filepath.IsAbs(absPath) {
		var err error
		absPath, err = tree.Abs(path)
		if err != nil {
			return false
		}
	}

	return syscall.Access(absPath, accessModeWrite) == nil
}

func isPathWritable(tree *synctree.Root, path string) bool {
	if isFileWritable(tree, path) {
		return true
	}

	parent := filepath.Dir(path)
	if parent == "." || parent == "/" {
		parent = ""
	}

	return isDirAccessibleForCapability(tree, parent, PermissionCapabilityLocalWrite)
}

// pathSetFromEvents builds a set of paths from scanner change events.
func pathSetFromEvents(events []ChangeEvent) map[string]bool {
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
func pathSetFromBatch(batch []PathChanges) map[string]bool {
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
