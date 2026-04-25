//go:build linux

package synctree

import "syscall"

func statDeviceID(stat *syscall.Stat_t) uint64 {
	return stat.Dev
}
