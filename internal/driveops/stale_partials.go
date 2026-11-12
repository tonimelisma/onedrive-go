package driveops

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// CleanStalePartials deletes onedrive-go-owned partial files found under the
// sync tree, leaving the subtrees opts excludes untouched. After a sync run
// completes, any surviving owned partial files are garbage: successful
// downloads rename them away, failed downloads delete them via
// removePartialIfNotCanceled, and context cancellation aborts the sync before
// this function runs. The only edge case is rename failure (B-207), where
// re-downloading on the next run is acceptable.
//
// Like SessionStore.CleanStale it goes on past a partial it cannot remove and
// returns every such failure, joined with a failed walk, beside the count it
// did remove. Reporting a failure is the caller's, which says one line for the
// lot: a line per file here was a Warn for every stuck file after every run.
func CleanStalePartials(
	tree *synctree.Root,
	logger *slog.Logger,
	opts TransferArtifactCleanupOptions,
) (int, error) {
	deleted := 0
	var failures []error
	skipDirs := normalizePartialCleanupSkipDirs(opts.SkipDirs)
	includedDirs := normalizePartialCleanupDirList(opts.IncludedDirs)

	err := tree.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Debug only: the local scanner owns reporting unreadable paths,
			// and this housekeeping walk runs at the end of a sync run, so a
			// Warn here repeated the same condition on every run.
			logger.Debug("couldn't check path for leftover download files",
				slog.String("path", path),
				slog.Any("error", walkErr),
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
			logger.Debug("failed to relativize partial file for cleanup",
				slog.String("path", path),
				slog.Any("error", relErr),
			)

			return nil
		}
		if !partialCleanupPathInIncludedScope(relPath, false, includedDirs) {
			return nil
		}

		removeErr := tree.Remove(relPath)
		switch {
		case removeErr == nil:
			logger.Info("deleted stale partial file", slog.String("path", relPath))

			deleted++
		case errors.Is(removeErr, fs.ErrNotExist):
			// Gone between the walk and the removal: nothing is left over,
			// and this cleanup did not remove it.
		default:
			failures = append(failures, fmt.Errorf("leftover download file: %w", removeErr))
		}

		return nil
	})
	if err != nil {
		failures = append(failures, fmt.Errorf("scanning for partial files: %w", err))
	}

	return deleted, errors.Join(failures...)
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
		logger.Debug("failed to relativize directory for partial cleanup",
			slog.String("path", dirPath),
			slog.Any("error", err),
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
