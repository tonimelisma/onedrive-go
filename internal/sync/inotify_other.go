//go:build !linux

package sync

import (
	"errors"
	"log/slog"
	"syscall"
)

// ReadInotifyLimit is a no-op on non-Linux platforms.
// FSEvents (macOS) and kqueue (BSD) have no per-directory watch limit.
func ReadInotifyLimit() (int, error) { return 0, nil }

// CheckInotifyCapacity is a no-op on non-Linux platforms.
func CheckInotifyCapacity(_ int, _ *slog.Logger) {}

// IsWatchLimitError checks for ENOSPC, which signals inotify watch limit
// exhaustion on Linux. On non-Linux platforms this is still checked so that
// the mock-based unit tests work correctly, but the error is never returned
// by real filesystem watchers (FSEvents/kqueue have no per-directory limit).
func IsWatchLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
