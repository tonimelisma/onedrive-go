//go:build darwin || freebsd || netbsd

package synctree

import (
	"io/fs"
	"syscall"
)

// statBirthTime returns the birth time stat(2) reports in st_birthtimespec,
// in Unix nanoseconds. APFS, HFS+, UFS2 and ZFS record one; a filesystem that
// does not reports zero or a negative time, both read as unknown.
func statBirthTime(stat *syscall.Stat_t) int64 {
	birthTime := stat.Birthtimespec.Nano()
	if birthTime <= 0 {
		return 0
	}

	return birthTime
}

// withBirthTime returns info unchanged: the stat these platforms return
// already carries the birth time IdentityFromFileInfo reads.
func withBirthTime(info fs.FileInfo, _ string) fs.FileInfo {
	return info
}
