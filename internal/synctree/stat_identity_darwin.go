//go:build darwin

package synctree

import "syscall"

func statDeviceID(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev)
}
