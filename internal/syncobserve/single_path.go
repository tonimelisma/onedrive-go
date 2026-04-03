package syncobserve

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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
	path := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(path))

	if skip := shouldObserveWithFilter(name, path, observedKindUnknown, filter, rules); skip != nil {
		if skip.Reason == "" {
			return SinglePathObservation{Resolved: true}, nil
		}

		return SinglePathObservation{Skipped: skip}, nil
	}

	absPath, err := syncTree.Abs(path)
	if err != nil {
		return SinglePathObservation{}, fmt.Errorf("observe single path %s: resolve: %w", path, err)
	}

	info, isSymlink, err := statObservedPath(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
			return SinglePathObservation{Resolved: true}, nil
		}

		return SinglePathObservation{}, fmt.Errorf("observe single path %s: stat: %w", path, err)
	}

	if shouldSkipObservedSymlink(isSymlink, filter) {
		return SinglePathObservation{Resolved: true}, nil
	}

	if skip := shouldObserveWithFilter(name, path, infoKind(info), filter, rules); skip != nil {
		if skip.Reason == "" {
			return SinglePathObservation{Resolved: true}, nil
		}

		return SinglePathObservation{Skipped: skip}, nil
	}

	itemType := synctypes.ItemTypeFile
	if info.IsDir() {
		itemType = synctypes.ItemTypeFolder
	}

	if itemType == synctypes.ItemTypeFile && info.Size() > MaxOneDriveFileSize {
		return SinglePathObservation{Skipped: &synctypes.SkippedItem{
			Path:     path,
			Reason:   synctypes.IssueFileTooLarge,
			Detail:   fmt.Sprintf("file size %d bytes exceeds 250 GB limit", info.Size()),
			FileSize: info.Size(),
		}}, nil
	}

	hash := ""
	if itemType == synctypes.ItemTypeFile {
		hash = observeSinglePathHash(logger, path, absPath, info, base, observeStartNano, hashFunc)
	}

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
	}, nil
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
