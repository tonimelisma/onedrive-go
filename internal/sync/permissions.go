package sync

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// isOnReadOnlyFilesystem reports whether absDir sits on a read-only mount.
// The mount flags from statfs (ST_RDONLY on Linux, MNT_RDONLY on macOS) are
// the authority: they cover a read-only bind mount of a writable filesystem.
// When statfs cannot answer, faccessat(W_OK) is the fallback; it returns
// EROFS for a read-only superblock before checking permission bits, so it
// does not depend on ownership or on running as root.
func isOnReadOnlyFilesystem(absDir string) bool {
	if readOnly, known := mountIsReadOnly(absDir); known {
		return readOnly
	}

	return errors.Is(unix.Access(absDir, unix.W_OK), unix.EROFS)
}
