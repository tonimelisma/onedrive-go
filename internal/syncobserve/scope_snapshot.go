package syncobserve

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// BuildScopeSnapshot discovers the currently active local marker directories
// and combines them with the configured sync scope. Marker discovery is
// limited to directories that could possibly intersect the configured
// sync_paths; marker files outside that reachable area cannot affect sync.
func BuildScopeSnapshot(
	ctx context.Context,
	tree *synctree.Root,
	cfg syncscope.Config,
	logger *slog.Logger,
) (syncscope.Snapshot, error) {
	baseSnapshot, err := syncscope.NewSnapshot(cfg, nil)
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("build base scope snapshot: %w", err)
	}

	if baseSnapshot.IgnoreMarker() == "" {
		return baseSnapshot, nil
	}

	markerDirs := make([]string, 0)

	var walk func(fsPath, relPath string)
	walk = func(fsPath, relPath string) {
		if ctx.Err() != nil {
			return
		}

		if !baseSnapshot.ShouldTraverseDir(relPath) {
			return
		}

		dirEntries, readErr := localpath.ReadDir(fsPath)
		if readErr != nil {
			if logger != nil {
				logger.Debug("scope snapshot: read dir failed",
					slog.String("path", relPath),
					slog.String("error", readErr.Error()))
			}
			return
		}

		for _, entry := range dirEntries {
			name := nfcNormalize(entry.Name())
			if name == baseSnapshot.IgnoreMarker() {
				markerDirs = append(markerDirs, relPath)
				return
			}
		}

		for _, entry := range dirEntries {
			if ctx.Err() != nil {
				return
			}

			if !entry.IsDir() {
				continue
			}

			childRelPath := joinObservedPath(relPath, nfcNormalize(entry.Name()))
			childFsPath := filepath.Join(fsPath, entry.Name())
			walk(childFsPath, childRelPath)
		}
	}

	walk(tree.Path(), "")
	if ctx.Err() != nil {
		return syncscope.Snapshot{}, fmt.Errorf("build scope snapshot: %w", ctx.Err())
	}

	snapshot, err := syncscope.NewSnapshot(cfg, markerDirs)
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("finalize scope snapshot: %w", err)
	}

	return snapshot, nil
}

func underObservedRoot(root, path string) bool {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	if cleanRoot == cleanPath {
		return true
	}

	return len(cleanPath) > len(cleanRoot) && cleanPath[:len(cleanRoot)] == cleanRoot &&
		cleanPath[len(cleanRoot)] == os.PathSeparator
}
