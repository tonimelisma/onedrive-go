package sync

import (
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
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

// isDirWritable returns true if the directory permits creating and removing
// a temporary file. This is the engine's affirmative local-write probe for
// persisted permission scopes.
func isDirWritable(tree *synctree.Root, dir string) bool {
	var (
		absDir string
		err    error
	)
	if filepath.IsAbs(dir) {
		absDir = dir
	} else {
		absDir, err = tree.Abs(dir)
		if err != nil {
			return false
		}
	}

	f, err := localpath.CreateTemp(absDir, ".onedrive-go-write-probe-*")
	if err != nil {
		return false
	}

	name := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		if removeErr := localpath.Remove(name); removeErr != nil {
			return false
		}
		return false
	}
	if removeErr := localpath.Remove(name); removeErr != nil {
		return false
	}

	return true
}

// pathSetFromLocalRows builds a set of observed local snapshot paths from a
// full scan result. Full-scan permission clearing should use current observed
// truth, not only the subset of rows that happened to emit legacy change
// events.
func pathSetFromLocalRows(rows []LocalStateRow) map[string]bool {
	if len(rows) == 0 {
		return nil
	}

	paths := make(map[string]bool, len(rows))
	for i := range rows {
		if rows[i].Path != "" {
			paths[rows[i].Path] = true
		}
	}

	return paths
}
