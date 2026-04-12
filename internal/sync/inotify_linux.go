//go:build linux

package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// inotifyCapacityThreshold is the fraction of max_user_watches that triggers
// a capacity warning. At 80% usage, the operator should increase the limit.
const inotifyCapacityThreshold = 0.8

// percentMultiplier converts a fraction (0.0–1.0) to a percentage (0–100).
const percentMultiplier = 100.0

// ReadInotifyLimit reads the current inotify max_user_watches from procfs.
// Returns 0 if procfs is unavailable (e.g., container without /proc mounted).
func ReadInotifyLimit() (int, error) {
	data, err := localpath.ReadFile("/proc/sys/fs/inotify/max_user_watches")
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

// CheckInotifyCapacity warns if the estimated directory count exceeds 80% of
// the inotify max_user_watches limit and points operators at the sysctl knob
// they need to raise before watch startup becomes fragile.
func CheckInotifyCapacity(estimatedDirs int, logger *slog.Logger) {
	// Read the current procfs value directly instead of caching it in global
	// process state. This path runs at observer startup and the extra read is
	// negligible compared with the clarity of having no package-level state.
	limit, err := ReadInotifyLimit()
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

// IsWatchLimitError returns true if the error is caused by exhaustion of the
// inotify watch limit (ENOSPC on Linux).
func IsWatchLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
