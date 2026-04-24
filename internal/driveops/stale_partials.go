package driveops

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// StalePartialCleanupOptions controls which local subtrees stale partial
// cleanup may touch. SkipDirs entries are rooted slash paths relative to the
// sync root.
type StalePartialCleanupOptions struct {
	SkipDirs []string
}

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
	return CleanStalePartialsWithOptions(tree, logger, StalePartialCleanupOptions{})
}

// CleanStalePartialsWithOptions deletes stale .partial files while leaving
// skipped local subtrees untouched.
func CleanStalePartialsWithOptions(
	tree *synctree.Root,
	logger *slog.Logger,
	opts StalePartialCleanupOptions,
) (int, error) {
	deleted := 0
	skipDirs := normalizePartialCleanupSkipDirs(opts.SkipDirs)

	err := tree.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logger.Warn("skipping path due to error",
				slog.String("path", path),
				slog.String("error", walkErr.Error()),
			)

			return nil
		}

		if d.IsDir() {
			if shouldSkipPartialCleanupDir(tree, path, skipDirs, logger) {
				return filepath.SkipDir
			}

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

func normalizePartialCleanupSkipDirs(skipDirs []string) map[string]struct{} {
	normalized := make(map[string]struct{}, len(skipDirs))
	for _, skipDir := range skipDirs {
		skipDir = path.Clean(strings.TrimPrefix(filepath.ToSlash(skipDir), "/"))
		if skipDir == "." || skipDir == "" {
			continue
		}

		normalized[skipDir] = struct{}{}
	}

	return normalized
}

func shouldSkipPartialCleanupDir(
	tree *synctree.Root,
	dirPath string,
	skipDirs map[string]struct{},
	logger *slog.Logger,
) bool {
	if len(skipDirs) == 0 {
		return false
	}

	relPath, err := tree.Rel(dirPath)
	if err != nil {
		logger.Warn("failed to relativize directory for partial cleanup",
			slog.String("path", dirPath),
			slog.String("error", err.Error()),
		)

		return false
	}

	relPath = path.Clean(filepath.ToSlash(relPath))
	if relPath == "." || relPath == "" {
		return false
	}

	_, skip := skipDirs[relPath]
	return skip
}
