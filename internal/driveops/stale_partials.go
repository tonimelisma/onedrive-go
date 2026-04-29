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
// sync root. IncludedDirs entries use the same root-relative ancestor/container
// semantics as sync include filters.
type StalePartialCleanupOptions struct {
	SkipDirs            []string
	IncludedDirs        []string
	IncludeJunkPartials bool
}

// CleanStalePartials deletes onedrive-go-owned partial files found under the
// sync tree. After a sync run completes, any surviving owned partial files are
// garbage: successful downloads rename them away, failed downloads delete them
// via removePartialIfNotCanceled, and context cancellation aborts the sync
// before this function runs. The only edge case is rename failure (B-207),
// where re-downloading on the next run is acceptable.
//
// Follows the CleanStale pattern: per-file errors are logged and skipped,
// returns (count, scanError). The caller logs a summary.
func CleanStalePartials(tree *synctree.Root, logger *slog.Logger) (int, error) {
	return CleanStalePartialsWithOptions(tree, logger, StalePartialCleanupOptions{})
}

// CleanStalePartialsWithOptions deletes stale owned partial files while
// leaving skipped local subtrees untouched.
func CleanStalePartialsWithOptions(
	tree *synctree.Root,
	logger *slog.Logger,
	opts StalePartialCleanupOptions,
) (int, error) {
	deleted := 0
	skipDirs := normalizePartialCleanupSkipDirs(opts.SkipDirs)
	includedDirs := normalizePartialCleanupDirList(opts.IncludedDirs)

	err := tree.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logger.Warn("skipping path due to error",
				slog.String("path", path),
				slog.String("error", walkErr.Error()),
			)

			return nil
		}

		if d.IsDir() {
			if shouldSkipPartialCleanupDir(tree, path, skipDirs, includedDirs, logger) {
				return filepath.SkipDir
			}

			return nil
		}

		name := filepath.Base(path)
		isJunkPartial := opts.IncludeJunkPartials && strings.HasSuffix(strings.ToLower(name), downloadPartialSuffix)
		if !IsOwnedTransferArtifactName(name) && !isJunkPartial {
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
		if !partialCleanupPathInIncludedScope(relPath, false, includedDirs) {
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
	normalizedList := normalizePartialCleanupDirList(skipDirs)
	normalized := make(map[string]struct{}, len(normalizedList))
	for _, skipDir := range normalizedList {
		normalized[skipDir] = struct{}{}
	}

	return normalized
}

func normalizePartialCleanupDirList(dirs []string) []string {
	normalized := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dir = path.Clean(strings.TrimPrefix(filepath.ToSlash(dir), "/"))
		if dir == "." || dir == "" {
			continue
		}

		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		normalized = append(normalized, dir)
	}

	return normalized
}

func shouldSkipPartialCleanupDir(
	tree *synctree.Root,
	dirPath string,
	skipDirs map[string]struct{},
	includedDirs []string,
	logger *slog.Logger,
) bool {
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

	if _, skip := skipDirs[relPath]; skip {
		return true
	}

	return !partialCleanupPathInIncludedScope(relPath, true, includedDirs)
}

func partialCleanupPathInIncludedScope(relPath string, isDir bool, includedDirs []string) bool {
	if len(includedDirs) == 0 {
		return true
	}

	relPath = path.Clean(filepath.ToSlash(relPath))
	if relPath == "." || relPath == "" {
		return isDir
	}

	for _, includedDir := range includedDirs {
		if relPath == includedDir {
			return isDir
		}
		if strings.HasPrefix(relPath, includedDir+"/") {
			return true
		}
		if isDir && strings.HasPrefix(includedDir, relPath+"/") {
			return true
		}
	}

	return false
}
