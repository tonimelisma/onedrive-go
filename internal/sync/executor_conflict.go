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
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

	// A directory cannot take a placeholder file's name: the rename failed
	// with ENOTDIR on every pass (D-059a).
	sourceIsDir := false
	if info, statErr := e.syncTree.Lstat(action.Path); statErr == nil {
		sourceIsDir = info.IsDir()
	}

	if sourceIsDir {
		return e.executeDirectoryConflictCopy(action, absPath)
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
	now := e.nowFunc()

	for ordinal := 1; ; ordinal++ {
		candidate := conflictCopyPathWithOrdinal(absPath, now, ordinal)

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
		e.logger.Warn("Could not remove the empty placeholder file "+rel+"; you can delete it",
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
	return conflictCopyPathWithOrdinal(absPath, now, 1)
}

// conflictCopyPathWithOrdinal is conflictCopyPath for the ordinal-th copy of
// the same second ("file.conflict-20260101-120000-2.txt" for ordinal 2). The
// whole name, marker, ordinal and extension included, stays within
// maxLocalNameBytes: the ordinal used to be appended after the stem had
// already been trimmed to exactly 255 bytes, so a second same-second copy of
// a long name was unwritable and blocked the download on every pass (D-030).
// An extension too long to leave room for the marker is trimmed too.
func conflictCopyPathWithOrdinal(absPath string, now time.Time, ordinal int) string {
	stem, ext := ConflictStemExt(filepath.Base(absPath))

	return composeConflictCopyPath(filepath.Dir(absPath), stem, ext, now, ordinal)
}

// conflictCopyDirPathWithOrdinal names a directory's conflict copy. A
// directory has no extension, so a dot in its name stays in the stem:
// "photos.2024" -> "photos.2024.conflict-20260101-120000".
func conflictCopyDirPathWithOrdinal(absPath string, now time.Time, ordinal int) string {
	return composeConflictCopyPath(filepath.Dir(absPath), filepath.Base(absPath), "", now, ordinal)
}

func composeConflictCopyPath(dir, stem, ext string, now time.Time, ordinal int) string {
	suffix := ".conflict-" + now.Format("20060102-150405")
	if ordinal > 1 {
		suffix += fmt.Sprintf("-%d", ordinal)
	}

	if maxExt := maxLocalNameBytes - len(suffix) - minConflictStemBytes; len(ext) > maxExt {
		ext = truncateUTF8(ext, maxExt)
	}
	if room := maxLocalNameBytes - len(suffix) - len(ext); len(stem) > room {
		stem = truncateUTF8(stem, room)
	}

	return filepath.Join(dir, stem+suffix+ext)
}

// minConflictStemBytes is how much of the original stem a conflict copy keeps
// when the extension alone would fill the name, so the copy still says what
// it is a copy of.
const minConflictStemBytes = 16

// truncateUTF8 cuts s to at most n bytes at a UTF-8 sequence boundary and
// never empties a non-empty string: when even the first character is longer
// than n, that character is kept whole. It cuts at boundaries rather than
// trimming until the result is valid UTF-8, which emptied a name containing
// an invalid byte before the cut point.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}

	cut := max(n, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return s[:size]
	}

	return s[:cut]
}

// maxLocalNameBytes is NAME_MAX on Linux and macOS filesystems.
const maxLocalNameBytes = 255

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

// conflictCopyDirAttempts bounds the same-second names tried for a directory's
// conflict copy.
const conflictCopyDirAttempts = 100

// executeDirectoryConflictCopy renames a directory to a unique conflict-copy
// name with a no-replace rename (renameat2 RENAME_NOREPLACE on Linux,
// renamex_np RENAME_EXCL on macOS): an entry that already holds a candidate
// name, an empty directory included, is never replaced, and the next ordinal
// is tried. A plain rename(2) atomically replaces an empty directory at the
// destination, and a placeholder cannot help: the directory cannot be renamed
// onto a reserved name without replacing it. The source's ancestors were
// checked for symlinks by ExecuteConflictCopy, and the candidate sits beside
// the source.
func (e *executor) executeDirectoryConflictCopy(action *Action, absPath string) actionOutcome {
	now := e.nowFunc()
	for ordinal := 1; ordinal <= conflictCopyDirAttempts; ordinal++ {
		candidate := conflictCopyDirPathWithOrdinal(absPath, now, ordinal)
		rel, err := e.syncTree.Rel(candidate)
		if err != nil {
			return e.failedOutcomeWithFailure(action, ActionConflictCopy,
				fmt.Errorf("relativizing conflict copy path %s: %w", filepath.Base(candidate), normalizeSyncTreePathError(err)),
				action.Path, permissionCapabilityLocalWrite)
		}
		renameErr := localpath.RenameNoReplace(absPath, candidate)
		if renameErr == nil {
			e.logger.Debug("saved conflict copy",
				slog.String("path", action.Path),
				slog.String("conflict_copy", filepath.Base(candidate)),
			)
			return actionOutcome{
				Action:  ActionConflictCopy,
				Success: true,
				Path:    action.Path,
				OldPath: rel,
				DriveID: e.resolveDriveID(action),
				ItemID:  action.ItemID,
			}
		}
		if errors.Is(renameErr, localpath.ErrTargetExists) {
			continue
		}

		return e.failedOutcomeWithFailure(action, ActionConflictCopy,
			fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(candidate), renameErr),
			action.Path, permissionCapabilityLocalWrite)
	}

	return e.failedOutcomeWithFailure(action, ActionConflictCopy,
		fmt.Errorf("no free conflict copy name for %s", action.Path), action.Path, permissionCapabilityLocalWrite)
}
