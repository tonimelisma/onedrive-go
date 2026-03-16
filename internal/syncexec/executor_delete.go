package syncexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// maxComponentLength is the maximum length of a single path component allowed
// by OneDrive (Windows NTFS limit). Duplicated from scanner constants to keep
// syncexec free of scanner imports.
const maxComponentLength = 255

// deviceNameWithDigitLen is the length of COM0-COM9, LPT0-LPT9 device names.
const deviceNameWithDigitLen = 4

// IsDisposable returns true for files that are safe to remove when they block
// a parent directory deletion. These are OS junk files, editor temps, and
// names invalid for OneDrive.
func IsDisposable(name string) bool {
	// OS junk files.
	lower := strings.ToLower(name)
	if lower == ".ds_store" || lower == "thumbs.db" || lower == "__macosx" {
		return true
	}

	// Apple resource forks (._filename).
	if strings.HasPrefix(name, "._") {
		return true
	}

	// Editor temps and partial downloads.
	if isAlwaysExcluded(name) {
		return true
	}

	// Names that can't be synced to OneDrive (desktop.ini, ~$doc.docx, etc.).
	if reason, _ := validateOneDriveName(name); reason != "" {
		return true
	}

	return false
}

// FindNonDisposable recursively checks a directory for non-disposable files.
// Returns the relative path to the first non-disposable file found, or ""
// if all contents are disposable.
func FindNonDisposable(dirPath string) string {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "" // can't read → treat as disposable (will fail on RemoveAll anyway)
	}

	for _, entry := range entries {
		if !IsDisposable(entry.Name()) {
			return entry.Name()
		}

		if entry.IsDir() {
			if sub := FindNonDisposable(filepath.Join(dirPath, entry.Name())); sub != "" {
				return entry.Name() + "/" + sub
			}
		}
	}

	return ""
}

// ExecuteLocalDelete removes a local file or folder with S4 safety:
// for files, verifies hash before delete; mismatch triggers conflict copy.
func (e *Executor) ExecuteLocalDelete(_ context.Context, action *synctypes.Action) synctypes.Outcome {
	absPath, err := ContainedPath(e.syncRoot, action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, err)
	}

	info, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		// Already gone — success.
		e.logger.Debug("local delete: already absent", slog.String("path", action.Path))
		return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
	}

	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("stat %s: %w", action.Path, err))
	}

	if info.IsDir() {
		return e.DeleteLocalFolder(action, absPath)
	}

	return e.DeleteLocalFile(action, absPath, info)
}

// DeleteLocalFolder removes an empty local directory.
// NOTE: There is an inherent TOCTOU race between ReadDir and Remove — a file
// could be created between the two calls. This is acceptable because the DAG
// guarantees child deletes complete before parent folder deletes, and new
// creations would be caught in the next sync pass.
func (e *Executor) DeleteLocalFolder(action *synctypes.Action, absPath string) synctypes.Outcome {
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("reading dir %s: %w", action.Path, err))
	}

	if len(entries) > 0 {
		// Check if all remaining entries are disposable (OS junk, temp files).
		// For directories, check recursively — a disposable-named directory
		// could contain non-disposable files that would be silently lost.
		var blockers []string
		for _, entry := range entries {
			entryPath := filepath.Join(absPath, entry.Name())
			if !IsDisposable(entry.Name()) {
				blockers = append(blockers, entry.Name())
			} else if entry.IsDir() {
				if nonDisp := FindNonDisposable(entryPath); nonDisp != "" {
					blockers = append(blockers, entry.Name()+"/"+nonDisp)
				}
			}
		}

		if len(blockers) > 0 {
			return e.failedOutcome(action, synctypes.ActionLocalDelete,
				fmt.Errorf("directory %s blocked by non-disposable files: %v", action.Path, blockers))
		}

		// All entries are disposable — remove them before deleting the folder.
		for _, entry := range entries {
			entryPath := filepath.Join(absPath, entry.Name())
			if rmErr := os.RemoveAll(entryPath); rmErr != nil {
				e.logger.Warn("failed to remove disposable file",
					slog.String("path", entryPath),
					slog.String("error", rmErr.Error()),
				)
			}
		}
	}

	// Try trash before permanent delete.
	if e.trashFunc != nil {
		if err := e.trashFunc(absPath); err != nil {
			e.logger.Warn("failed to trash folder, falling back to permanent delete",
				slog.String("path", action.Path), slog.String("error", err.Error()))
		} else {
			e.logger.Debug("moved folder to trash", slog.String("path", action.Path))
			return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
		}
	}

	if err := os.Remove(absPath); err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("removing dir %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted local folder", slog.String("path", action.Path))

	return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
}

// DeleteLocalFile removes a file after verifying its hash matches baseline.
// Hash mismatch means the file was modified since the planner ran — rename
// to conflict copy and record as edit-delete conflict (B-133).
func (e *Executor) DeleteLocalFile(action *synctypes.Action, absPath string, info os.FileInfo) synctypes.Outcome {
	baselineHash := ""
	baselineRemoteHash := ""

	if action.View != nil && action.View.Baseline != nil {
		baselineHash = action.View.Baseline.LocalHash
		baselineRemoteHash = action.View.Baseline.RemoteHash
	}

	// S4 safety: verify hash before delete.
	if baselineHash != "" {
		currentHash, err := e.hashFunc(absPath)
		if err != nil {
			return e.failedOutcome(action, synctypes.ActionLocalDelete,
				fmt.Errorf("hashing %s before delete: %w", action.Path, err))
		}

		if currentHash != baselineHash {
			// File was modified — save as conflict copy instead of deleting.
			conflictPath := ConflictCopyPath(absPath, e.nowFunc())
			if renameErr := os.Rename(absPath, conflictPath); renameErr != nil {
				return e.failedOutcome(action, synctypes.ActionLocalDelete,
					fmt.Errorf("renaming modified file to conflict copy %s: %w", conflictPath, renameErr))
			}

			e.logger.Warn("local delete: hash mismatch, saved conflict copy",
				slog.String("path", action.Path),
				slog.String("conflict_copy", filepath.Base(conflictPath)),
			)

			// Return a conflict outcome so the conflict is tracked in the
			// conflicts table and visible via `conflicts list`.
			var remoteMtime int64
			if action.View != nil && action.View.Remote != nil {
				remoteMtime = action.View.Remote.Mtime
			}

			return synctypes.Outcome{
				Action:       synctypes.ActionConflict,
				Success:      true,
				Path:         action.Path,
				DriveID:      e.resolveDriveID(action),
				ItemID:       action.ItemID,
				ItemType:     synctypes.ItemTypeFile,
				ConflictType: synctypes.ConflictEditDelete,
				LocalHash:    currentHash,
				RemoteHash:   baselineRemoteHash,
				Mtime:        info.ModTime().UnixNano(),
				RemoteMtime:  remoteMtime,
			}
		}
	}

	// Try trash before permanent delete.
	if e.trashFunc != nil {
		if err := e.trashFunc(absPath); err != nil {
			e.logger.Warn("failed to trash file, falling back to permanent delete",
				slog.String("path", action.Path), slog.String("error", err.Error()))
		} else {
			e.logger.Debug("moved file to trash", slog.String("path", action.Path))
			return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
		}
	}

	if err := os.Remove(absPath); err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted local file", slog.String("path", action.Path))

	return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
}

// ExecuteRemoteDelete removes an item from OneDrive. 404 is treated as
// success (item already deleted).
func (e *Executor) ExecuteRemoteDelete(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	driveID := e.resolveDriveID(action)

	err := e.items.DeleteItem(ctx, driveID, action.ItemID)
	if err != nil {
		// 404 means already deleted — success.
		if errors.Is(err, graph.ErrNotFound) {
			e.logger.Debug("remote delete: already absent", slog.String("path", action.Path))
			return e.DeleteOutcome(action, synctypes.ActionRemoteDelete)
		}

		return e.failedOutcome(action, synctypes.ActionRemoteDelete, fmt.Errorf("deleting remote %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted remote item", slog.String("path", action.Path), slog.String("item_id", action.ItemID))

	return e.DeleteOutcome(action, synctypes.ActionRemoteDelete)
}

// DeleteOutcome builds a successful Outcome for a delete action.
func (e *Executor) DeleteOutcome(action *synctypes.Action, actionType synctypes.ActionType) synctypes.Outcome {
	return synctypes.Outcome{
		Action:   actionType,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: resolveActionItemType(action),
	}
}

// ---------------------------------------------------------------------------
// Name validation helpers (duplicated from scanner to avoid import cycle)
// ---------------------------------------------------------------------------

// isAlwaysExcluded returns true for file patterns that must never be synced.
// These are S7 safety invariants: partial downloads and editor temporaries.
//
// Called on every fsnotify event and every file during FullScan, so we use
// AsciiLower to avoid the heap allocation that strings.ToLower incurs per call.
// Suffixes are inlined as explicit checks — no slice allocation, no mutable
// package-level state, and the compiler inlines the string constants.
func isAlwaysExcluded(name string) bool {
	lower := AsciiLower(name)

	// Extension-based: partial downloads and editor temps.
	if strings.HasSuffix(lower, ".partial") ||
		strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".swp") ||
		strings.HasSuffix(lower, ".crdownload") {
		return true
	}

	// Prefix-based: editor backup files (~file) and LibreOffice locks (.~lock).
	if strings.HasPrefix(name, "~") || strings.HasPrefix(name, ".~") {
		return true
	}

	return false
}

// AsciiLower returns s with ASCII uppercase letters converted to lowercase.
// Unlike strings.ToLower, this avoids heap allocation when s is already
// lowercase (the common case for filenames). Non-ASCII bytes are passed through
// unchanged, which is correct for file extension matching.
func AsciiLower(s string) string {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			// Found an uppercase letter — allocate and convert.
			buf := make([]byte, len(s))
			copy(buf, s[:i])

			for j := i; j < len(s); j++ {
				if s[j] >= 'A' && s[j] <= 'Z' {
					buf[j] = s[j] + ('a' - 'A')
				} else {
					buf[j] = s[j]
				}
			}

			return string(buf)
		}
	}

	// No uppercase letters found — return the original string (zero alloc).
	return s
}

// validateOneDriveName checks whether a filename is valid for OneDrive.
// Returns a non-empty reason string if the name is invalid.
// Duplicated from scanner to avoid import cycle between syncexec and sync.
func validateOneDriveName(name string) (reason, detail string) {
	if name == "" {
		return synctypes.IssueInvalidFilename, "empty filename"
	}

	if name[len(name)-1] == '.' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q ends with a period", name)
	}

	if name[len(name)-1] == ' ' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q ends with a space", name)
	}

	if name[0] == ' ' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q starts with a space", name)
	}

	if len(name) > maxComponentLength {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q exceeds %d-character component limit", name, maxComponentLength)
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q is a reserved Windows device name", name)
	}

	if isReservedPattern(name, lower) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q matches a reserved OneDrive pattern", name)
	}

	if containsInvalidChars(name) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q contains characters forbidden by OneDrive", name)
	}

	return "", ""
}

// isReservedDeviceName returns true for Windows reserved device names
// (case-insensitive): CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9.
func isReservedDeviceName(lower string) bool {
	switch lower {
	case "con", "prn", "aux", "nul":
		return true
	}

	// COM0-COM9, LPT0-LPT9: exactly 4 characters, prefix + single digit.
	if len(lower) == deviceNameWithDigitLen &&
		(strings.HasPrefix(lower, "com") || strings.HasPrefix(lower, "lpt")) {
		digit := lower[3]
		return digit >= '0' && digit <= '9'
	}

	return false
}

// isReservedPattern returns true for OneDrive-specific reserved file patterns:
// .lock extension, desktop.ini, ~$ prefix (Office temp), _vti_ substring.
func isReservedPattern(name, lower string) bool {
	if strings.HasSuffix(lower, ".lock") {
		return true
	}

	if lower == "desktop.ini" {
		return true
	}

	if strings.HasPrefix(name, "~$") {
		return true
	}

	return strings.Contains(lower, "_vti_")
}

// containsInvalidChars returns true if the name contains characters
// forbidden by OneDrive: " * : < > ? / \ |
func containsInvalidChars(name string) bool {
	for _, c := range name {
		switch c {
		case '"', '*', ':', '<', '>', '?', '/', '\\', '|':
			return true
		}
	}

	return false
}
