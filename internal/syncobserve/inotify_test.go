package syncobserve

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Cross-platform smoke tests: verify inotify functions compile and don't panic.

func TestReadInotifyLimit_NoPanic(t *testing.T) {
	t.Parallel()

	limit, err := ReadInotifyLimit()
	// On Linux: limit > 0, err == nil. On other platforms: limit == 0, err == nil.
	if err != nil {
		t.Logf("ReadInotifyLimit returned error (ok on non-Linux): %v", err)
	}

	t.Logf("inotify limit: %d", limit)
}

func TestCheckInotifyCapacity_NoPanic(t *testing.T) {
	t.Parallel()

	// Must not panic regardless of platform.
	CheckInotifyCapacity(100, slog.Default())
}

func TestIsWatchLimitError_NoPanic(t *testing.T) {
	t.Parallel()

	// Must not panic; returns false on non-Linux.
	result := IsWatchLimitError(nil)
	assert.False(t, result, "IsWatchLimitError(nil) should return false")
}
