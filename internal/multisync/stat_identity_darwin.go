//go:build darwin

package multisync

import "syscall"

func statDeviceID(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev)
}
