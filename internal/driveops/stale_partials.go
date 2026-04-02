package driveops

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// CleanStalePartials deletes all .partial files found under the sync tree.
// After a sync run completes, any surviving .partial files are guaranteed
// garbage: successful downloads rename them away, failed downloads delete
// them via removePartialIfNotCanceled, and context cancellation aborts the
// sync before this function runs. The only edge case is rename failure
// (B-207), where re-downloading on the next run is acceptable.
//
// Follows the CleanStale pattern: per-file errors are logged and skipped,
// returns (count, scanError). The caller logs a summary.
func CleanStalePartials(tree *synctree.Root, logger *slog.Logger) (int, error) {
	deleted := 0

	err := tree.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
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

		relPath, relErr := tree.Rel(path)
		if relErr != nil {
			logger.Warn("failed to relativize partial file for cleanup",
				slog.String("path", path),
				slog.String("error", relErr.Error()),
			)

			return nil
		}

		if removeErr := tree.Remove(relPath); removeErr != nil && !os.IsNotExist(removeErr) {
			logger.Warn("failed to delete partial file",
				slog.String("path", relPath),
				slog.String("error", removeErr.Error()),
			)

			return nil
		}

		logger.Info("deleted stale partial file", slog.String("path", relPath))

		deleted++

		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("scanning for partial files: %w", err)
	}

	return deleted, nil
}
