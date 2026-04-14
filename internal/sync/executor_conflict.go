package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExecuteConflict handles conflict resolution. For edit-delete conflicts,
// the modified local version wins automatically (industry consensus). For
// all others, keep_both: rename local to a conflict copy, download remote.
func (e *Executor) ExecuteConflict(ctx context.Context, action *Action) ActionOutcome {
	// Edit-delete: local was modified, remote was deleted. Upload local file
	// to re-create remote, record as auto-resolved. No conflict copy needed.
	if action.ConflictInfo != nil && action.ConflictInfo.ConflictType == ConflictEditDelete {
		return e.ExecuteEditDeleteConflict(ctx, action)
	}

	absPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedConflictOutcome(
			action,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	// Step 1: Rename local to conflict copy (if it exists).
	conflictPath, err := e.uniqueConflictCopyPath(absPath)
	if err != nil {
		return e.failedConflictOutcome(action, err, action.Path, PermissionCapabilityLocalWrite)
	}
	conflictRel, err := e.syncTree.Rel(conflictPath)
	if err != nil {
		return e.failedConflictOutcome(
			action,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}
	localExists := true

	if err := e.syncTree.Rename(action.Path, conflictRel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Local file absent — skip rename, proceed to download.
			localExists = false
		} else {
			return e.failedConflictOutcome(
				action,
				fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(conflictPath), normalizeSyncTreePathError(err)),
				action.Path,
				PermissionCapabilityLocalWrite,
			)
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
			if restoreErr := e.syncTree.Rename(conflictRel, action.Path); restoreErr != nil {
				e.logger.Error("failed to restore local after download failure",
					slog.String("path", action.Path),
					slog.String("error", restoreErr.Error()),
				)
			}
		}

		return e.failedConflictOutcome(
			action,
			fmt.Errorf("downloading remote during conflict resolution for %s: %w", action.Path, downloadOutcome.Error),
			conflictFailurePath(&downloadOutcome, action.Path),
			conflictFailureCapability(&downloadOutcome, PermissionCapabilityLocalWrite, PermissionCapabilityRemoteRead),
		)
	}

	// Build conflict outcome from the download result.
	o := downloadOutcome
	o.Action = ActionConflict

	if action.ConflictInfo != nil {
		o.ConflictType = action.ConflictInfo.ConflictType
	}

	return o
}

// uniqueConflictCopyPath returns the first available conflict-copy path for an
// on-disk file. Executor-owned uniqueness is intentional: readability comes
// from the timestamped base name, but actual collision prevention depends on
// the current sync-root filesystem state.
func (e *Executor) uniqueConflictCopyPath(absPath string) (string, error) {
	basePath := ConflictCopyPath(absPath, e.nowFunc())
	available, err := e.conflictCopyPathAvailable(basePath)
	if err != nil {
		return "", err
	}
	if available {
		return basePath, nil
	}

	dir := filepath.Dir(basePath)
	name := filepath.Base(basePath)
	stem, ext := ConflictStemExt(name)

	for ordinal := 2; ; ordinal++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, ordinal, ext))
		available, candidateErr := e.conflictCopyPathAvailable(candidate)
		if candidateErr != nil {
			return "", candidateErr
		}
		if available {
			return candidate, nil
		}
	}
}

func (e *Executor) conflictCopyPathAvailable(absPath string) (bool, error) {
	relPath, err := e.syncTree.Rel(absPath)
	if err != nil {
		return false, fmt.Errorf("relativizing conflict copy path %s: %w", filepath.Base(absPath), normalizeSyncTreePathError(err))
	}

	_, err = e.syncTree.Stat(relPath)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}

	return false, fmt.Errorf("stating conflict copy path %s: %w", filepath.Base(absPath), normalizeSyncTreePathError(err))
}

// ExecuteEditDeleteConflict auto-resolves edit-delete conflicts by uploading
// the locally modified file to re-create it on the remote side. The local
// version wins automatically — industry consensus (rclone, Dropbox, Google
// Drive, OneDrive official, abraunegg).
func (e *Executor) ExecuteEditDeleteConflict(ctx context.Context, action *Action) ActionOutcome {
	e.logger.Info("auto-resolving edit-delete conflict: local edit wins",
		slog.String("path", action.Path),
	)

	uploadOutcome := e.ExecuteUpload(ctx, action)
	if !uploadOutcome.Success {
		return e.failedConflictOutcome(
			action,
			fmt.Errorf("uploading local during edit-delete auto-resolve for %s: %w", action.Path, uploadOutcome.Error),
			conflictFailurePath(&uploadOutcome, action.Path),
			conflictFailureCapability(&uploadOutcome, PermissionCapabilityLocalRead, PermissionCapabilityRemoteWrite),
		)
	}

	// Build conflict outcome from the upload result.
	o := uploadOutcome
	o.Action = ActionConflict
	o.ConflictType = ConflictEditDelete
	o.ResolvedBy = ResolvedByAuto

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

func (e *Executor) failedConflictOutcome(
	action *Action,
	err error,
	failurePath string,
	failureCapability PermissionCapability,
) ActionOutcome {
	outcome := e.failedOutcome(action, ActionConflict, err)
	if failurePath != "" {
		outcome.FailurePath = failurePath
	}
	outcome.FailureCapability = failureCapability

	return outcome
}

func conflictFailurePath(outcome *ActionOutcome, fallback string) string {
	if outcome == nil {
		return fallback
	}
	if outcome.FailurePath != "" {
		return outcome.FailurePath
	}
	if outcome.Path != "" {
		return outcome.Path
	}

	return fallback
}

func conflictFailureCapability(
	outcome *ActionOutcome,
	localCapability PermissionCapability,
	remoteCapability PermissionCapability,
) PermissionCapability {
	if outcome == nil {
		return PermissionCapabilityUnknown
	}
	if outcome.FailureCapability != PermissionCapabilityUnknown {
		return outcome.FailureCapability
	}
	if errors.Is(outcome.Error, os.ErrPermission) {
		return localCapability
	}
	if ExtractHTTPStatus(outcome.Error) == http.StatusForbidden {
		return remoteCapability
	}

	return PermissionCapabilityUnknown
}
