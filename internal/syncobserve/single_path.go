package syncobserve

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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
	syncRoot string,
	relPath string,
	base *synctypes.BaselineEntry,
	observeStartNano int64,
	hashFunc func(string) (string, error),
) (SinglePathObservation, error) {
	path := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(path))

	if skip := ShouldObserve(name, path); skip != nil {
		if skip.Reason == "" {
			return SinglePathObservation{Resolved: true}, nil
		}

		return SinglePathObservation{Skipped: skip}, nil
	}

	absPath := filepath.Join(syncRoot, path)
	info, err := trustedStat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
			return SinglePathObservation{Resolved: true}, nil
		}

		return SinglePathObservation{}, fmt.Errorf("observe single path %s: stat: %w", path, err)
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

	var hash string
	if itemType == synctypes.ItemTypeFile {
		if CanReuseBaselineHash(info, base, observeStartNano) {
			hash = base.LocalHash
		} else {
			if hashFunc == nil {
				hashFunc = driveops.ComputeQuickXorHash
			}

			hash, err = computeStableHashWith(absPath, hashFunc)
			if err != nil {
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
				hash = ""
			}
		}
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
