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

// SinglePathObservation rebuilds local truth for one path without invoking a
// full scan or watch pipeline. Used by engine-owned retry/trial work to keep
// single-path reconstruction aligned with normal local observation semantics.
type SinglePathObservation struct {
	Event    *ChangeEvent
	Skipped  *SkippedItem
	Resolved bool
}

// ObserveSinglePath rebuilds the current local state for a single sync path.
// It mirrors the parts of normal local observation that matter for retry/trial
// reconstruction: observation filters, oversized-file rejection, baseline hash
// reuse, and "emit with empty hash" behavior when hashing fails.
func ObserveSinglePath(
	logger *slog.Logger,
	syncTree *synctree.Root,
	relPath string,
	base *BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
) (SinglePathObservation, error) {
	return ObserveSinglePathWithFilter(
		logger,
		syncTree,
		relPath,
		base,
		observeStartNano,
		hashFunc,
		LocalFilterConfig{},
		LocalObservationRules{},
	)
}

// ObserveSinglePathWithFilter applies the same single-path reconstruction as
// ObserveSinglePath, but with explicit local filter configuration and
// platform-derived observation rules from the engine. Retry/trial work uses
// this so configured exclusions and drive-type-specific validation stay
// aligned with full-scan and watch semantics.
func ObserveSinglePathWithFilter(
	logger *slog.Logger,
	syncTree *synctree.Root,
	relPath string,
	base *BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
	filter LocalFilterConfig,
	rules LocalObservationRules,
) (SinglePathObservation, error) {
	path := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(path))

	if observation, resolved := resolveSinglePathWithoutStat(name, path, filter, rules); resolved {
		return observation, nil
	}

	absPath, info, isSymlink, err := statSingleObservedPath(syncTree, path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return SinglePathObservation{Skipped: &SkippedItem{
				Path:   path,
				Reason: IssueLocalReadDenied,
				Detail: "file not accessible (check filesystem permissions)",
			}}, nil
		}
		return SinglePathObservation{}, err
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
	filter LocalFilterConfig,
	rules LocalObservationRules,
) (SinglePathObservation, bool) {
	if skip := shouldObserveWithFilter(name, path, observedKindUnknown, filter, rules); skip != nil {
		if skip.Reason == "" {
			return SinglePathObservation{Resolved: true}, true
		}

		return SinglePathObservation{Skipped: skip}, true
	}

	return SinglePathObservation{}, false
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

func resolveSinglePathWithInfo(
	name string,
	path string,
	info os.FileInfo,
	isSymlink bool,
	filter LocalFilterConfig,
	rules LocalObservationRules,
) (SinglePathObservation, bool) {
	if info == nil {
		return SinglePathObservation{Resolved: true}, true
	}

	if shouldSkipObservedSymlink(isSymlink, filter) {
		return SinglePathObservation{Resolved: true}, true
	}

	if skip := shouldObserveWithFilter(name, path, infoKind(info), filter, rules); skip != nil {
		if skip.Reason == "" {
			return SinglePathObservation{Resolved: true}, true
		}

		return SinglePathObservation{Skipped: skip}, true
	}

	if info.IsDir() || info.Size() <= MaxOneDriveFileSize {
		return SinglePathObservation{}, false
	}

	return SinglePathObservation{Skipped: &SkippedItem{
		Path:     path,
		Reason:   IssueFileTooLarge,
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
) SinglePathObservation {
	return SinglePathObservation{
		Event: &ChangeEvent{
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
	if CanReuseBaselineHash(info, base, observeStartNano) {
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
		if errors.Is(err, ErrFileChangedDuringHash) {
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
