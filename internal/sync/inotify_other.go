//go:build !linux

package sync

import "log/slog"

// readInotifyLimit is a no-op on non-Linux platforms. fsnotify uses kqueue on
// macOS and the BSDs (it has no FSEvents backend), and kqueue has no
// inotify-style max_user_watches counter to read. Its real limit is different:
// kqueue holds an open file descriptor for every watched directory and every
// file inside one, so a large tree is bounded by the process's open-file limit.
func readInotifyLimit() (int, error) { return 0, nil }

// checkInotifyCapacity is a no-op on non-Linux platforms. fsnotify uses
// kqueue on macOS and the BSDs, which has no inotify-style watch limit but
// holds one open descriptor per watched directory and file; that budget is
// checked separately.
func checkInotifyCapacity(_ int, _ *slog.Logger) {}

// watchCreateEMFILECause: without inotify, EMFILE from watcher creation is the
// open-file limit.
const watchCreateEMFILECause = watchCauseFileLimit
