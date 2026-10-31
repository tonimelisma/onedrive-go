//go:build !linux && !darwin && !freebsd && !netbsd

package synctree

import (
	"io/fs"
	"os"
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

// withDescriptorBirthTime returns info unchanged on platforms without a birth
// time.
func withDescriptorBirthTime(info fs.FileInfo, _ *os.File) fs.FileInfo {
	return info
}
