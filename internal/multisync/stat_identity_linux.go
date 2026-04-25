//go:build linux

package multisync

import "syscall"

func statDeviceID(stat *syscall.Stat_t) uint64 {
	return stat.Dev
}
