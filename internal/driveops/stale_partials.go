package driveops

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// CleanStalePartials deletes all .partial files found under syncRoot.
// After a sync run completes, any surviving .partial files are guaranteed
// garbage: successful downloads rename them away, failed downloads delete
// them via removePartialIfNotCanceled, and context cancellation aborts the
// sync before this function runs. The only edge case is rename failure
// (B-207), where re-downloading on the next run is acceptable.
//
// Follows the CleanStale pattern: per-file errors are logged and skipped,
// returns (count, scanError). The caller logs a summary.
func CleanStalePartials(syncRoot string, logger *slog.Logger) (int, error) {
	// Pre-check: return error for nonexistent root (WalkDir would swallow it).
	if _, err := os.Stat(syncRoot); err != nil {
		return 0, fmt.Errorf("scanning for partial files: %w", err)
	}

	deleted := 0

	err := filepath.WalkDir(syncRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			rel, relErr := filepath.Rel(syncRoot, path)
			if relErr != nil {
				rel = path
			}

			logger.Warn("skipping path due to error",
				slog.String("path", rel),
				slog.String("error", err.Error()),
			)

			return nil
		}

		if d.IsDir() {
			return nil
		}

		if filepath.Ext(path) != ".partial" {
			return nil
		}

		rel, relErr := filepath.Rel(syncRoot, path)
		if relErr != nil {
			rel = path
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("failed to delete partial file",
				slog.String("path", rel),
				slog.String("error", err.Error()),
			)

			return nil
		}

		logger.Info("deleted stale partial file", slog.String("path", rel))

		deleted++

		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("scanning for partial files: %w", err)
	}

	return deleted, nil
}
