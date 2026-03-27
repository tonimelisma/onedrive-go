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
	root, err := os.OpenRoot(syncRoot)
	if err != nil {
		return 0, fmt.Errorf("opening sync root: %w", err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			logger.Warn("closing sync root after partial cleanup failed",
				slog.String("root", syncRoot),
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	deleted := 0

	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logger.Warn("skipping path due to error",
				slog.String("path", path),
				slog.String("error", walkErr.Error()),
			)

			return nil
		}

		if d.IsDir() {
			return nil
		}

		if filepath.Ext(path) != ".partial" {
			return nil
		}

		if removeErr := root.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			logger.Warn("failed to delete partial file",
				slog.String("path", path),
				slog.String("error", removeErr.Error()),
			)

			return nil
		}

		logger.Info("deleted stale partial file", slog.String("path", path))

		deleted++

		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("scanning for partial files: %w", err)
	}

	return deleted, nil
}
