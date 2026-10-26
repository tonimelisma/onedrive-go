//go:build !linux

package sync

import (
	"errors"
	"log/slog"
	"syscall"
)

// readInotifyLimit is a no-op on non-Linux platforms. fsnotify uses kqueue on
// macOS and the BSDs (it has no FSEvents backend), and kqueue has no
// inotify-style max_user_watches counter to read. Its real limit is different:
// kqueue holds an open file descriptor for every watched directory and every
// file inside one, so a large tree is bounded by the process's open-file limit.
func readInotifyLimit() (int, error) { return 0, nil }

// checkInotifyCapacity is a no-op on non-Linux platforms.
func checkInotifyCapacity(_ int, _ *slog.Logger) {}

// isWatchLimitError checks for ENOSPC, which signals inotify watch limit
// exhaustion on Linux. On non-Linux platforms this is still checked so that
// the mock-based unit tests work correctly, but the kqueue backend never
// returns it: running out of descriptors there surfaces as EMFILE, which this
// check does not classify as a watch-limit error.
func isWatchLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
