package syncobserve

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// SinglePathObservation rebuilds local truth for one path without invoking a
// full scan or watch pipeline. Used by engine-owned retry/trial work to keep
// single-path reconstruction aligned with normal local observation semantics.
type SinglePathObservation struct {
	Event    *synctypes.ChangeEvent
	Skipped  *synctypes.SkippedItem
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
	base *synctypes.BaselineEntry,
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
		synctypes.LocalFilterConfig{},
		synctypes.LocalObservationRules{},
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
	base *synctypes.BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
	filter synctypes.LocalFilterConfig,
	rules synctypes.LocalObservationRules,
) (SinglePathObservation, error) {
	return ObserveSinglePathWithScope(
		logger,
		syncTree,
		relPath,
		base,
		observeStartNano,
		hashFunc,
		filter,
		rules,
		syncscope.Snapshot{},
	)
}

// ObserveSinglePathWithScope applies the canonical single-path reconstruction
// semantics together with the effective bidirectional sync scope. Paths
// outside the active scope resolve silently because the engine no longer owns
// them for planning.
func ObserveSinglePathWithScope(
	logger *slog.Logger,
	syncTree *synctree.Root,
	relPath string,
	base *synctypes.BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
	filter synctypes.LocalFilterConfig,
	rules synctypes.LocalObservationRules,
	scopeSnapshot syncscope.Snapshot,
) (SinglePathObservation, error) {
	path := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(path))

	if observation, resolved := resolveSinglePathWithoutStat(name, path, filter, rules, scopeSnapshot); resolved {
		return observation, nil
	}

	absPath, info, isSymlink, err := statSingleObservedPath(syncTree, path)
	if err != nil {
		return SinglePathObservation{}, err
	}

	if observation, resolved := resolveSinglePathWithInfo(name, path, info, isSymlink, filter, rules, scopeSnapshot); resolved {
		return observation, nil
	}

	itemType := singlePathItemType(info)
	hash := ""
	if itemType == synctypes.ItemTypeFile {
		hash = observeSinglePathHash(logger, path, absPath, info, base, observeStartNano, hashFunc)
	}

	return singlePathEvent(path, name, itemType, info, hash), nil
}

func resolveSinglePathWithoutStat(
	name string,
	path string,
	filter synctypes.LocalFilterConfig,
	rules synctypes.LocalObservationRules,
	scopeSnapshot syncscope.Snapshot,
) (SinglePathObservation, bool) {
	if scopeSnapshot.IsMarkerFile(path) {
		return SinglePathObservation{Resolved: true}, true
	}

	if !scopeSnapshot.AllowsPath(path) && !scopeSnapshot.ShouldTraverseDir(path) {
		return SinglePathObservation{Resolved: true}, true
	}

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

		return "", nil, false, fmt.Errorf("observe single path %s: stat: %w", path, err)
	}

	return absPath, info, isSymlink, nil
}

func resolveSinglePathWithInfo(
	name string,
	path string,
	info os.FileInfo,
	isSymlink bool,
	filter synctypes.LocalFilterConfig,
	rules synctypes.LocalObservationRules,
	scopeSnapshot syncscope.Snapshot,
) (SinglePathObservation, bool) {
	if info == nil {
		return SinglePathObservation{Resolved: true}, true
	}

	if shouldSkipObservedSymlink(isSymlink, filter) {
		return SinglePathObservation{Resolved: true}, true
	}

	if !singlePathAllowedByScope(path, info, scopeSnapshot) {
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

	return SinglePathObservation{Skipped: &synctypes.SkippedItem{
		Path:     path,
		Reason:   synctypes.IssueFileTooLarge,
		Detail:   fmt.Sprintf("file size %d bytes exceeds 250 GB limit", info.Size()),
		FileSize: info.Size(),
	}}, true
}

func singlePathAllowedByScope(path string, info os.FileInfo, scopeSnapshot syncscope.Snapshot) bool {
	if info.IsDir() {
		return scopeSnapshot.ShouldTraverseDir(path) && scopeSnapshot.AllowsPath(path)
	}

	return scopeSnapshot.AllowsPath(path)
}

func singlePathItemType(info os.FileInfo) synctypes.ItemType {
	if info.IsDir() {
		return synctypes.ItemTypeFolder
	}

	return synctypes.ItemTypeFile
}

func singlePathEvent(
	path string,
	name string,
	itemType synctypes.ItemType,
	info os.FileInfo,
	hash string,
) SinglePathObservation {
	return SinglePathObservation{
		Event: &synctypes.ChangeEvent{
			Source:   synctypes.SourceLocal,
			Type:     synctypes.ChangeModify,
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
	base *synctypes.BaselineEntry,
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
		if errors.Is(err, synctypes.ErrFileChangedDuringHash) {
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
