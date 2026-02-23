package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// executeConflict handles keep_both conflict resolution: rename local to
// a timestamped conflict copy, then download remote to the original path.
func (e *Executor) executeConflict(ctx context.Context, action *Action) Outcome {
	absPath := filepath.Join(e.syncRoot, action.Path)

	// Step 1: Rename local to conflict copy (if it exists).
	conflictPath := conflictCopyPath(absPath, e.nowFunc())
	localExists := true

	if err := os.Rename(absPath, conflictPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Edit-delete conflict: local file is gone, skip rename.
			localExists = false
		} else {
			return e.failedOutcome(action, ActionConflict,
				fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(conflictPath), err))
		}
	}

	if localExists {
		e.logger.Debug("saved conflict copy",
			slog.String("path", action.Path),
			slog.String("conflict_copy", filepath.Base(conflictPath)),
		)
	}

	// Step 2: Download remote to original path.
	downloadOutcome := e.executeDownload(ctx, action)
	if !downloadOutcome.Success {
		// Download failed — restore local from conflict copy if we moved it.
		if localExists {
			if restoreErr := os.Rename(conflictPath, absPath); restoreErr != nil {
				e.logger.Error("failed to restore local after download failure",
					slog.String("path", action.Path),
					slog.String("error", restoreErr.Error()),
				)
			}
		}

		return e.failedOutcome(action, ActionConflict,
			fmt.Errorf("downloading remote during conflict resolution for %s: %w", action.Path, downloadOutcome.Error))
	}

	// Build conflict outcome from the download result.
	o := downloadOutcome
	o.Action = ActionConflict

	if action.ConflictInfo != nil {
		o.ConflictType = action.ConflictInfo.ConflictType
	}

	return o
}

// conflictCopyPath generates a timestamped conflict copy path.
// "file.txt" -> "file.conflict-20260101-120000.txt"
// ".bashrc"  -> ".bashrc.conflict-20260101-120000" (dotfile: no separate ext)
func conflictCopyPath(absPath string, now time.Time) string {
	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)
	stem, ext := conflictStemExt(name)
	ts := now.Format("20060102-150405")

	return filepath.Join(dir, fmt.Sprintf("%s.conflict-%s%s", stem, ts, ext))
}

// conflictStemExt splits a filename into stem and extension, handling the
// dotfile edge case where filepath.Ext returns the entire name for files
// like ".bashrc" (LEARNINGS §2).
func conflictStemExt(name string) (string, string) {
	// Dotfile with no other dots: treat entire name as stem, no extension.
	if name != "" && name[0] == '.' && strings.Count(name, ".") == 1 {
		return name, ""
	}

	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]

	return stem, ext
}
