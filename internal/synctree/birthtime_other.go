//go:build !linux && !darwin && !freebsd && !netbsd

package synctree

import (
	"io/fs"
	"syscall"
)

// statBirthTime reports no birth time on platforms without a known source;
// identity there is (device, inode) alone.
func statBirthTime(*syscall.Stat_t) int64 {
	return 0
}

// withBirthTime returns info unchanged on platforms without a birth time.
func withBirthTime(info fs.FileInfo, _ string) fs.FileInfo {
	return info
}
