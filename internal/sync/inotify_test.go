package sync

import (
	"log/slog"
	"testing"
)

// Cross-platform smoke tests: verify inotify functions compile and don't panic.

func TestReadInotifyLimit_NoPanic(t *testing.T) {
	t.Parallel()

	limit, err := readInotifyLimit()
	// On Linux: limit > 0, err == nil. On other platforms: limit == 0, err == nil.
	if err != nil {
		t.Logf("readInotifyLimit returned error (ok on non-Linux): %v", err)
	}

	t.Logf("inotify limit: %d", limit)
}

func TestCheckInotifyCapacity_NoPanic(t *testing.T) {
	t.Parallel()

	// Must not panic regardless of platform.
	checkInotifyCapacity(100, slog.Default())
}

func TestIsWatchLimitError_NoPanic(t *testing.T) {
	t.Parallel()

	// Must not panic; returns false on non-Linux.
	result := isWatchLimitError(nil)
	if result {
		t.Error("isWatchLimitError(nil) should return false")
	}
}
