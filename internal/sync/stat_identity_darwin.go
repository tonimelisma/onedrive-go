//go:build darwin

package sync

import "syscall"

func statDeviceID(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev)
}
