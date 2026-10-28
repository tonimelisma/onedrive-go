//go:build darwin

package synctree

import (
	"io/fs"
	"syscall"
)

// ChangeTimeFromFileInfo returns the inode change time (ctime) of info in
// Unix nanoseconds. The kernel sets ctime on every content write, metadata
// change and rename, and no unprivileged call can set it back, so it records
// an in-place edit that restored the old mtime (`cp -p`, `touch -r`). It is a
// change detector only, never a content identity.
func ChangeTimeFromFileInfo(info fs.FileInfo) (int64, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}

	return stat.Ctimespec.Nano(), true
}
