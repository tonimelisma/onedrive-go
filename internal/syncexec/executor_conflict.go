package syncexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ExecuteConflict handles conflict resolution. For edit-delete conflicts,
// the modified local version wins automatically (industry consensus). For
// all others, keep_both: rename local to a conflict copy, download remote.
func (e *Executor) ExecuteConflict(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	// Edit-delete: local was modified, remote was deleted. Upload local file
	// to re-create remote, record as auto-resolved. No conflict copy needed.
	if action.ConflictInfo != nil && action.ConflictInfo.ConflictType == synctypes.ConflictEditDelete {
		return e.ExecuteEditDeleteConflict(ctx, action)
	}

	absPath, err := ContainedPath(e.syncRoot, action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionConflict, err)
	}

	// Step 1: Rename local to conflict copy (if it exists).
	conflictPath := ConflictCopyPath(absPath, e.nowFunc())
	localExists := true

	if err := os.Rename(absPath, conflictPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Local file absent — skip rename, proceed to download.
			localExists = false
		} else {
			return e.failedOutcome(action, synctypes.ActionConflict,
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
	downloadOutcome := e.ExecuteDownload(ctx, action)
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

		return e.failedOutcome(action, synctypes.ActionConflict,
			fmt.Errorf("downloading remote during conflict resolution for %s: %w", action.Path, downloadOutcome.Error))
	}

	// Build conflict outcome from the download result.
	o := downloadOutcome
	o.Action = synctypes.ActionConflict

	if action.ConflictInfo != nil {
		o.ConflictType = action.ConflictInfo.ConflictType
	}

	return o
}

// ExecuteEditDeleteConflict auto-resolves edit-delete conflicts by uploading
// the locally modified file to re-create it on the remote side. The local
// version wins automatically — industry consensus (rclone, Dropbox, Google
// Drive, OneDrive official, abraunegg).
func (e *Executor) ExecuteEditDeleteConflict(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	e.logger.Info("auto-resolving edit-delete conflict: local edit wins",
		slog.String("path", action.Path),
	)

	uploadOutcome := e.ExecuteUpload(ctx, action)
	if !uploadOutcome.Success {
		return e.failedOutcome(action, synctypes.ActionConflict,
			fmt.Errorf("uploading local during edit-delete auto-resolve for %s: %w",
				action.Path, uploadOutcome.Error))
	}

	// Build conflict outcome from the upload result.
	o := uploadOutcome
	o.Action = synctypes.ActionConflict
	o.ConflictType = synctypes.ConflictEditDelete
	o.ResolvedBy = synctypes.ResolvedByAuto

	if action.View != nil && action.View.Remote != nil {
		o.RemoteMtime = action.View.Remote.Mtime
	}

	return o
}

// ConflictCopyPath generates a timestamped conflict copy path.
// "file.txt" -> "file.conflict-20260101-120000.txt"
// ".bashrc"  -> ".bashrc.conflict-20260101-120000" (dotfile: no separate ext)
func ConflictCopyPath(absPath string, now time.Time) string {
	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)
	stem, ext := ConflictStemExt(name)
	ts := now.Format("20060102-150405")

	return filepath.Join(dir, fmt.Sprintf("%s.conflict-%s%s", stem, ts, ext))
}

// ConflictStemExt splits a filename into stem and extension, handling the
// dotfile edge case where filepath.Ext returns the entire name for files
// like ".bashrc" (LEARNINGS §2).
//
// For files with multiple extensions (e.g., "archive.tar.gz"), only the last
// extension is separated: stem="archive.tar", ext=".gz". This matches
// filepath.Ext behavior and produces "archive.tar.conflict-YYYYMMDD-HHMMSS.gz".
func ConflictStemExt(name string) (string, string) {
	// Dotfile with no other dots: treat entire name as stem, no extension.
	if name != "" && name[0] == '.' && strings.Count(name, ".") == 1 {
		return name, ""
	}

	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]

	return stem, ext
}
