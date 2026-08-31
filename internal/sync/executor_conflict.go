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

// conflictCopyReservationPerm is the mode of the placeholder that claims a
// conflict-copy name. The rename replaces it immediately, so it only has to
// be private while it exists.
const conflictCopyReservationPerm = 0o600

// ExecuteConflictCopy preserves the local canonical file by renaming it to a
// unique conflict-copy path. It performs no baseline or remote mutation; the
// current-state planner schedules any follow-up download/upload action
// separately.
func (e *executor) ExecuteConflictCopy(_ context.Context, action *Action) actionOutcome {
	if err := e.validateNoSymlinkBoundary(action.Path, "conflict-copy"); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			err,
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	absPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			normalizeSyncTreePathError(err),
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	conflictPath, conflictRel, err := e.reserveUniqueConflictCopyPath(absPath)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionConflictCopy, err, action.Path, permissionCapabilityLocalWrite)
	}

	if err := e.syncTree.Rename(action.Path, conflictRel); err != nil {
		e.releaseConflictCopyReservation(conflictRel)

		if errors.Is(err, os.ErrNotExist) {
			outcome := actionOutcome{
				Action:   ActionConflictCopy,
				Success:  true,
				Path:     action.Path,
				DriveID:  e.resolveDriveID(action),
				ItemID:   action.ItemID,
				OldPath:  action.Path,
				ParentID: e.resolvedParentIDForOutcome(action, nil),
			}
			return outcome
		}

		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(conflictPath), normalizeSyncTreePathError(err)),
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	e.logger.Debug("saved conflict copy",
		slog.String("path", action.Path),
		slog.String("conflict_copy", filepath.Base(conflictPath)),
	)

	outcome := actionOutcome{
		Action:  ActionConflictCopy,
		Success: true,
		Path:    action.Path,
		OldPath: conflictRel,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
	return outcome
}

// reserveUniqueConflictCopyPath claims a conflict-copy destination and
// returns it as both an absolute and a sync-root-relative path.
//
// The name is claimed by creating it exclusively rather than by finding a path
// that does not exist yet. Stat-then-rename is a check-then-act: rename
// silently replaces whatever sits at the destination, so anything that
// appeared in the gap -- a second conflict copy in the same second, another
// mount over the same tree, the user -- was destroyed by the very operation
// whose purpose is to preserve a file. An exclusive create makes the claim
// atomic against every other creator of that name.
//
// The caller owns the reservation and must release it if the rename does not
// happen.
func (e *executor) reserveUniqueConflictCopyPath(absPath string) (string, string, error) {
	basePath := conflictCopyPath(absPath, e.nowFunc())

	rel, reserved, err := e.reserveConflictCopyPath(basePath)
	if err != nil {
		return "", "", err
	}
	if reserved {
		return basePath, rel, nil
	}

	dir := filepath.Dir(basePath)
	name := filepath.Base(basePath)
	stem, ext := ConflictStemExt(name)

	for ordinal := 2; ; ordinal++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, ordinal, ext))

		rel, reserved, err := e.reserveConflictCopyPath(candidate)
		if err != nil {
			return "", "", err
		}
		if reserved {
			return candidate, rel, nil
		}
	}
}

// releaseConflictCopyReservation drops a claim that will not be used. The
// reservation is an empty placeholder, so leaving one behind would look like a
// truncated conflict copy to the user.
func (e *executor) releaseConflictCopyReservation(rel string) {
	if err := e.syncTree.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logger.Warn("removing unused conflict copy reservation",
			slog.String("conflict_copy", filepath.Base(rel)),
			slog.String("error", err.Error()),
		)
	}
}

func (e *executor) reserveConflictCopyPath(absPath string) (string, bool, error) {
	relPath, err := e.syncTree.Rel(absPath)
	if err != nil {
		return "", false, fmt.Errorf("relativizing conflict copy path %s: %w", filepath.Base(absPath), normalizeSyncTreePathError(err))
	}

	f, err := e.syncTree.OpenFile(relPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, conflictCopyReservationPerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return relPath, false, nil
		}

		return "", false, fmt.Errorf("reserving conflict copy path %s: %w", filepath.Base(absPath), normalizeSyncTreePathError(err))
	}

	if closeErr := f.Close(); closeErr != nil {
		e.releaseConflictCopyReservation(relPath)

		return "", false, fmt.Errorf("closing conflict copy reservation %s: %w", filepath.Base(absPath), closeErr)
	}

	return relPath, true, nil
}

// conflictCopyPath generates a timestamped conflict copy path.
// "file.txt" -> "file.conflict-20260101-120000.txt"
// ".bashrc"  -> ".bashrc.conflict-20260101-120000" (dotfile: no separate ext)
func conflictCopyPath(absPath string, now time.Time) string {
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
