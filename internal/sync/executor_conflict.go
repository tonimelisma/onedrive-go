package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ExecuteConflictCopy preserves the local canonical file or folder by renaming
// it to a unique conflict-copy path. It performs no baseline or remote
// mutation; the current-state planner schedules any follow-up download/upload
// action separately.
func (e *Executor) ExecuteConflictCopy(_ context.Context, action *Action) ActionOutcome {
	// The entry is renamed from its spelling on disk; the copy is a new entry,
	// named from the stored name inside the parents' spellings on disk
	// (R-2.13.2).
	source := e.syncTree.Spelling(action.Path)
	if err := e.validateNoSymlinkBoundary(source, "conflict-copy"); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionConflictCopy,
			err,
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	// A folder's copy keeps a dot in its name in the stem; a file's copy
	// keeps its extension last. A source that is gone takes the file's
	// naming, and its rename reports it gone; a source that cannot be checked
	// is a failure, not a guess that it is a file.
	candidateName := conflictCopyPathWithOrdinal
	info, statErr := e.syncTree.Lstat(source)
	switch {
	case statErr == nil && info.IsDir():
		candidateName = conflictCopyDirPathWithOrdinal
	case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
		return e.failedOutcomeWithFailure(action, ActionConflictCopy,
			fmt.Errorf("checking conflict copy source: %w", normalizeSyncTreePathError(statErr)),
			action.Path, PermissionCapabilityLocalWrite)
	}

	return e.renameToConflictCopy(action, source, e.syncTree.SpellingOfParent(action.Path), candidateName)
}

// vanishedConflictCopySourceOutcome is the outcome of a conflict copy whose
// source is gone before its rename, a file or a folder alike: nothing is left
// to preserve, so the copy succeeds for the path it was asked about.
func (e *Executor) vanishedConflictCopySourceOutcome(action *Action) ActionOutcome {
	return ActionOutcome{
		Action:   ActionConflictCopy,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		OldPath:  action.Path,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}
}

// conflictCopyPath generates a timestamped conflict copy path beside path.
// "file.txt" -> "file.conflict-20260101-120000.txt"
// ".bashrc"  -> ".bashrc.conflict-20260101-120000" (dotfile: no separate ext)
func conflictCopyPath(path string, now time.Time) string {
	return conflictCopyPathWithOrdinal(path, now, 1)
}

// conflictCopyPathWithOrdinal is conflictCopyPath for the ordinal-th copy of
// the same second ("file.conflict-20260101-120000-2.txt" for ordinal 2). The
// whole name, marker, ordinal and extension included, stays within
// maxLocalNameBytes: the stem is trimmed to the room the marker and the
// ordinal leave, so a second same-second copy of a long name is as writable
// as the first (D-030). An extension too long to leave room for the marker is
// trimmed too.
func conflictCopyPathWithOrdinal(path string, now time.Time, ordinal int) string {
	stem, ext := ConflictStemExt(filepath.Base(path))

	return composeConflictCopyPath(filepath.Dir(path), stem, ext, now, ordinal)
}

// conflictCopyDirPathWithOrdinal names a directory's conflict copy beside
// namingRel, the sync-folder-relative path the copy is named from. A
// directory has no extension, so a dot in its name stays in the stem:
// "photos.2024" -> "photos.2024.conflict-20260101-120000".
func conflictCopyDirPathWithOrdinal(namingRel string, now time.Time, ordinal int) string {
	return composeConflictCopyPath(filepath.Dir(namingRel), filepath.Base(namingRel), "", now, ordinal)
}

// conflictCopyStampLayout is the second a conflict copy's name carries.
const conflictCopyStampLayout = "20060102-150405"

func composeConflictCopyPath(dir, stem, ext string, now time.Time, ordinal int) string {
	suffix := ".conflict-" + now.Format(conflictCopyStampLayout)
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

// conflictCopyAttempts bounds the same-second names a conflict copy tries.
const conflictCopyAttempts = 100

// renameToConflictCopy renames source, a file or a folder as spelled on disk,
// to the first free conflict-copy name with the sync tree's no-replace
// rename: an entry that already holds a candidate name, an empty folder
// included, is never replaced, and the next ordinal is tried. naming is the
// sync-folder-relative path the copy's name is derived from (the stored
// name, R-2.13.2), and candidateName builds the ordinal-th candidate beside
// it. Nothing is created before the rename, so a copy that fails leaves no
// name behind. The rename is made on descriptors of both parent folders
// opened inside the sync folder, so a folder swapped for a symlink after
// ExecuteConflictCopy checked the source's ancestors fails the rename instead
// of carrying it outside, and it never copies the entry, so a file keeps its
// identity and a program writing to it keeps writing to the copy.
func (e *Executor) renameToConflictCopy(
	action *Action, source, naming string, candidateName func(string, time.Time, int) string,
) ActionOutcome {
	now := e.nowFunc()
	for ordinal := 1; ordinal <= conflictCopyAttempts; ordinal++ {
		candidate := candidateName(naming, now, ordinal)
		renameErr := e.syncTree.RenameNoReplace(source, candidate)
		switch {
		case renameErr == nil:
		case errors.Is(renameErr, synctree.ErrSourceNotRemoved):
			// The copy is at its name, which is what the conflict copy is
			// for; the source's name may still name the same file.
			e.logger.Warn("Saved the conflict copy "+candidate+", but could not remove the original name "+source,
				slog.String("path", source),
				slog.Any("error", renameErr),
			)
		case errors.Is(renameErr, synctree.ErrTargetExists):
			continue
		default:
			return e.failedConflictCopyRename(action, source, candidate, renameErr)
		}

		e.logger.Debug("saved conflict copy",
			slog.String("path", action.Path),
			slog.String("conflict_copy", filepath.Base(candidate)),
		)
		return ActionOutcome{
			Action:  ActionConflictCopy,
			Success: true,
			Path:    action.Path,
			OldPath: candidate,
			DriveID: e.resolveDriveID(action),
			ItemID:  action.ItemID,
		}
	}

	return e.failedOutcomeWithFailure(action, ActionConflictCopy,
		fmt.Errorf("no free conflict copy name for %s: all %d names stamped %s are taken",
			action.Path, conflictCopyAttempts, now.Format(conflictCopyStampLayout)),
		action.Path, PermissionCapabilityLocalWrite)
}

// failedConflictCopyRename is the outcome of a conflict copy whose rename
// failed with renameErr. The rename's ENOENT may name a missing parent as
// well as the source, so only a source a lstat through the sync tree finds
// absent is the vanished outcome; a lstat that fails another way is added to
// the failure on the same line, so its reason is not lost.
func (e *Executor) failedConflictCopyRename(action *Action, source, candidate string, renameErr error) ActionOutcome {
	if errors.Is(renameErr, os.ErrNotExist) {
		_, statErr := e.syncTree.Lstat(source)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			return e.vanishedConflictCopySourceOutcome(action)
		case statErr != nil:
			renameErr = fmt.Errorf("%w; checking conflict copy source: %w", renameErr, statErr)
		}
	}

	return e.failedOutcomeWithFailure(action, ActionConflictCopy,
		fmt.Errorf("renaming to conflict copy %s: %w", filepath.Base(candidate), normalizeSyncTreePathError(renameErr)),
		action.Path, PermissionCapabilityLocalWrite)
}
