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

// ExecuteConflictCopy preserves the local canonical file by renaming it to a
// unique conflict-copy path. It performs no baseline or remote mutation; the
// current-state planner schedules any follow-up download/upload action
// separately.
func (e *Executor) ExecuteConflictCopy(_ context.Context, action *Action) ActionOutcome {
	absPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	conflictPath, err := e.uniqueConflictCopyPath(absPath)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionConflictCopy, err, action.Path, PermissionCapabilityLocalWrite)
	}
	conflictRel, err := e.syncTree.Rel(conflictPath)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	if err := e.syncTree.Rename(action.Path, conflictRel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			outcome := ActionOutcome{
				Action:   ActionConflictCopy,
				Success:  true,
				Path:     action.Path,
				DriveID:  e.resolveDriveID(action),
				ItemID:   action.ItemID,
				OldPath:  action.Path,
				ParentID: resolvedUploadParentID(action, nil),
			}
			decorateConflictOutcome(action, &outcome)
			return outcome
		}

		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(conflictPath), normalizeSyncTreePathError(err)),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	e.logger.Debug("saved conflict copy",
		slog.String("path", action.Path),
		slog.String("conflict_copy", filepath.Base(conflictPath)),
	)

	outcome := ActionOutcome{
		Action:  ActionConflictCopy,
		Success: true,
		Path:    action.Path,
		OldPath: conflictRel,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
	decorateConflictOutcome(action, &outcome)
	return outcome
}

func decorateConflictOutcome(action *Action, outcome *ActionOutcome) {
	if action == nil || action.ConflictInfo == nil || outcome == nil {
		return
	}

	outcome.ConflictType = action.ConflictInfo.ConflictType
	if action.ConflictInfo.ConflictType == ConflictEditDelete && outcome.Action == ActionUpload {
		outcome.ResolvedBy = ResolvedByAuto
	}
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
