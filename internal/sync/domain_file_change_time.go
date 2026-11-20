package sync

import (
	"io/fs"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// FileChangeTime returns the ctime a local_state row records for info: the
// inode change time for a regular file, and zero (unknown) for anything else,
// because a folder's ctime moves with every child and means nothing here.
func FileChangeTime(info fs.FileInfo) int64 {
	if info == nil || !info.Mode().IsRegular() {
		return 0
	}
	ctime, ok := synctree.ChangeTimeFromFileInfo(info)
	if !ok {
		return 0
	}

	return ctime
}
