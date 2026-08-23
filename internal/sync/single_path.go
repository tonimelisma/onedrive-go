package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// singlePathObservation rebuilds local truth for one path without invoking a
// full scan or watch pipeline. Used by engine-owned retry/trial work to keep
// single-path reconstruction aligned with normal local observation semantics.
type singlePathObservation struct {
	Event    *changeEvent
	Skipped  *skippedItem
	Resolved bool
}

// observeSinglePathWithFilter rebuilds the current local state for a single
// sync path with explicit local filter configuration and
// platform-derived observation rules from the engine. Retry/trial work uses
// this so configured exclusions and drive-type-specific validation stay
// aligned with full-scan and watch semantics.
//
// production caller; see the single-path wiring discrepancy recorded in
// spec/design/sync-observation.md. Do not drop the parameter: it is threaded
// into observeSinglePathHash and will carry a real logger once the retry/trial
// path is wired.
//
//nolint:unparam // logger is only ever nil today because this family has no
func observeSinglePathWithFilter(
	logger *slog.Logger,
	syncTree *synctree.Root,
	relPath string,
	base *BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
	filter ContentFilterConfig,
	rules LocalObservationRules,
) (singlePathObservation, error) {
	path := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(path))

	if observation, resolved := resolveSinglePathWithoutStat(name, path, filter, rules); resolved {
		return observation, nil
	}

	absPath, info, isSymlink, err := statSingleObservedPath(syncTree, path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return singlePathObservation{
				Skipped: singlePathPermissionDeniedSkippedItem(syncTree, path, absPath),
			}, nil
		}
		return singlePathObservation{}, err
	}

	if observation, resolved := resolveSinglePathWithInfo(name, path, info, isSymlink, filter, rules); resolved {
		return observation, nil
	}

	itemType := singlePathItemType(info)
	hash := ""
	if itemType == ItemTypeFile {
		hash = observeSinglePathHash(logger, path, absPath, info, base, observeStartNano, hashFunc)
	}

	return singlePathEvent(path, name, itemType, info, hash), nil
}

func resolveSinglePathWithoutStat(
	name string,
	path string,
	filter ContentFilterConfig,
	rules LocalObservationRules,
) (singlePathObservation, bool) {
	if skip := shouldObserveWithFilter(name, path, observedKindUnknown, filter, nil, rules); skip != nil {
		if skip.Reason == "" {
			return singlePathObservation{Resolved: true}, true
		}

		return singlePathObservation{Skipped: skip}, true
	}

	return singlePathObservation{}, false
}

func statSingleObservedPath(syncTree *synctree.Root, path string) (string, os.FileInfo, bool, error) {
	absPath, err := syncTree.Abs(path)
	if err != nil {
		return "", nil, false, fmt.Errorf("observe single path %s: resolve: %w", path, err)
	}

	info, isSymlink, err := statObservedPath(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
			return "", nil, false, nil
		}
		if errors.Is(err, os.ErrPermission) {
			return absPath, nil, false, os.ErrPermission
		}

		return "", nil, false, fmt.Errorf("observe single path %s: stat: %w", path, err)
	}

	return absPath, info, isSymlink, nil
}

func singlePathPermissionDeniedSkippedItem(
	syncTree *synctree.Root,
	path string,
	absPath string,
) *skippedItem {
	if syncTree != nil && absPath != "" {
		parentDir := filepath.Dir(absPath)
		if !isDirAccessible(syncTree, parentDir) {
			boundary := deepestDeniedObservedBoundary(syncTree, parentDir)
			if relBoundary, err := syncTree.Rel(boundary); err == nil {
				return &skippedItem{
					Path:               nfcNormalize(filepath.ToSlash(relBoundary)),
					Reason:             issueLocalReadDenied,
					Detail:             "directory not accessible (check filesystem permissions)",
					BlocksReadBoundary: true,
				}
			}
		}
	}

	return &skippedItem{
		Path:   path,
		Reason: issueLocalReadDenied,
		Detail: "file not accessible (check filesystem permissions)",
	}
}

func deepestDeniedObservedBoundary(syncTree *synctree.Root, parentDir string) string {
	boundary := parentDir
	for {
		parent := filepath.Dir(boundary)
		if parent == boundary {
			return boundary
		}
		if isDirAccessible(syncTree, parent) {
			return boundary
		}
		boundary = parent
	}
}

func resolveSinglePathWithInfo(
	name string,
	path string,
	info os.FileInfo,
	isSymlink bool,
	filter ContentFilterConfig,
	rules LocalObservationRules,
) (singlePathObservation, bool) {
	if info == nil {
		return singlePathObservation{Resolved: true}, true
	}

	if shouldSkipObservedSymlink(isSymlink, filter) {
		return singlePathObservation{Resolved: true}, true
	}

	if skip := shouldObserveWithFilter(name, path, infoKind(info), filter, nil, rules); skip != nil {
		if skip.Reason == "" {
			return singlePathObservation{Resolved: true}, true
		}

		return singlePathObservation{Skipped: skip}, true
	}

	if info.IsDir() || info.Size() <= maxOneDriveFileSize {
		return singlePathObservation{}, false
	}

	return singlePathObservation{Skipped: &skippedItem{
		Path:     path,
		Reason:   issueFileTooLarge,
		Detail:   fmt.Sprintf("file size %d bytes exceeds 250 GB limit", info.Size()),
		FileSize: info.Size(),
	}}, true
}

func singlePathItemType(info os.FileInfo) ItemType {
	if info.IsDir() {
		return ItemTypeFolder
	}

	return ItemTypeFile
}

func singlePathEvent(
	path string,
	name string,
	itemType ItemType,
	info os.FileInfo,
	hash string,
) singlePathObservation {
	return singlePathObservation{
		Event: &changeEvent{
			Source:   SourceLocal,
			Type:     ChangeModify,
			Path:     path,
			Name:     name,
			ItemType: itemType,
			Size:     info.Size(),
			Hash:     hash,
			Mtime:    info.ModTime().UnixNano(),
		},
	}
}

func infoKind(info os.FileInfo) observedKind {
	if info.IsDir() {
		return observedKindDir
	}

	return observedKindFile
}

func observeSinglePathHash(
	logger *slog.Logger,
	path string,
	absPath string,
	info os.FileInfo,
	base *BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
) string {
	if canReuseBaselineHash(info, base, observeStartNano) {
		return base.LocalHash
	}

	if hashFunc == nil {
		hashFunc = driveops.ComputeQuickXorHash
	}

	hash, err := computeStableHashWith(absPath, hashFunc)
	if err == nil {
		return hash
	}

	if logger != nil {
		if errors.Is(err, errFileChangedDuringHash) {
			logger.Debug("file metadata still settling, emitting with empty hash",
				slog.String("path", path))
		} else {
			logger.Warn("hash failed, emitting event with empty hash",
				slog.String("path", path),
				slog.String("error", err.Error()))
		}
	}

	return ""
}
