//go:build linux

package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	stdsync "sync"
	"syscall"
)

// inotifyCapacityThreshold is the fraction of max_user_watches that triggers
// a capacity warning. At 80% usage, the operator should increase the limit.
const inotifyCapacityThreshold = 0.8

// percentMultiplier converts a fraction (0.0–1.0) to a percentage (0–100).
const percentMultiplier = 100.0

// cachedInotifyLimit caches the inotify max_user_watches value for the
// process lifetime. The limit is effectively constant — runtime sysctl
// changes require a daemon restart to take effect.
var cachedInotifyLimit = stdsync.OnceValues(readInotifyLimit)

// readInotifyLimit reads the current inotify max_user_watches from procfs.
// Returns 0 if procfs is unavailable (e.g., container without /proc mounted).
func readInotifyLimit() (int, error) {
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("sync: reading inotify limit: %w", err)
	}

	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("sync: parsing inotify limit: %w", err)
	}

	return limit, nil
}

// checkInotifyCapacity warns if the estimated directory count exceeds 80% of
// the inotify max_user_watches limit. Provides sysctl advice per MULTIDRIVE.md §9.1.
func checkInotifyCapacity(estimatedDirs int, logger *slog.Logger) {
	limit, err := cachedInotifyLimit()
	if err != nil {
		logger.Warn("failed to read inotify watch limit",
			slog.String("error", err.Error()))

		return
	}

	if limit == 0 {
		return
	}

	usage := float64(estimatedDirs) / float64(limit)
	if usage >= inotifyCapacityThreshold {
		logger.Warn("inotify watch usage near limit — consider increasing",
			slog.Int("estimated_dirs", estimatedDirs),
			slog.Int("max_user_watches", limit),
			slog.String("usage", fmt.Sprintf("%.0f%%", usage*percentMultiplier)),
			slog.String("advice", "sudo sysctl -w fs.inotify.max_user_watches=524288"),
		)
	}
}

// isWatchLimitError returns true if the error is caused by exhaustion of the
// inotify watch limit (ENOSPC on Linux).
func isWatchLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
