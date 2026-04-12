package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

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
	if IsAlwaysExcluded(name) {
		return true
	}

	// Names that can't be synced to OneDrive (desktop.ini, ~$doc.docx, etc.).
	if reason, _ := ValidateOneDriveName(name); reason != "" {
		return true
	}

	return false
}

// FindNonDisposable recursively checks a directory for non-disposable files.
// Returns the relative path to the first non-disposable file found, or ""
// if all contents are disposable.
func FindNonDisposable(tree *synctree.Root, dirPath string) string {
	entries, err := tree.ReadDir(dirPath)
	if err != nil {
		return "" // can't read → treat as disposable (will fail on RemoveAll anyway)
	}

	for _, entry := range entries {
		if !IsDisposable(entry.Name()) {
			return entry.Name()
		}

		if entry.IsDir() {
			if sub := FindNonDisposable(tree, filepath.Join(dirPath, entry.Name())); sub != "" {
				return entry.Name() + "/" + sub
			}
		}
	}

	return ""
}

// ExecuteLocalDelete removes a local file or folder with S4 safety:
// for files, verifies hash before delete; mismatch triggers conflict copy.
func (e *Executor) ExecuteLocalDelete(_ context.Context, action *synctypes.Action) synctypes.Outcome {
	info, err := e.syncTree.Stat(action.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already gone — success.
			e.logger.Debug("local delete: already absent", slog.String("path", action.Path))
			return e.DeleteOutcome(action, synctypes.ActionLocalDelete)
		}

		return e.failedOutcome(action, synctypes.ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	absPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, normalizeSyncTreePathError(err))
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
	relPath, err := e.syncTree.Rel(absPath)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	entries, err := e.syncTree.ReadDir(relPath)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("reading dir %s: %w", action.Path, err))
	}

	if len(entries) > 0 {
		// Check if all remaining entries are disposable (OS junk, temp files).
		// For directories, check recursively — a disposable-named directory
		// could contain non-disposable files that would be silently lost.
		var blockers []string
		for _, entry := range entries {
			entryPath := filepath.Join(relPath, entry.Name())
			if !IsDisposable(entry.Name()) {
				blockers = append(blockers, entry.Name())
			} else if entry.IsDir() {
				if nonDisp := FindNonDisposable(e.syncTree, entryPath); nonDisp != "" {
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
			entryPath := filepath.Join(relPath, entry.Name())
			if rmErr := e.syncTree.RemoveAll(entryPath); rmErr != nil {
				e.logger.Warn("failed to remove disposable file",
					slog.String("path", entryPath),
					slog.String("error", normalizeSyncTreePathError(rmErr).Error()),
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

	if err := e.syncTree.Remove(relPath); err != nil {
		return e.failedOutcome(
			action,
			synctypes.ActionLocalDelete,
			fmt.Errorf("removing dir %s: %w", action.Path, normalizeSyncTreePathError(err)),
		)
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
			conflictPath, pathErr := e.uniqueConflictCopyPath(absPath)
			if pathErr != nil {
				return e.failedOutcome(action, synctypes.ActionLocalDelete, pathErr)
			}
			conflictRel, relErr := e.syncTree.Rel(conflictPath)
			if relErr != nil {
				return e.failedOutcome(action, synctypes.ActionLocalDelete, normalizeSyncTreePathError(relErr))
			}

			if renameErr := e.syncTree.Rename(action.Path, conflictRel); renameErr != nil {
				return e.failedOutcome(action, synctypes.ActionLocalDelete,
					fmt.Errorf("renaming modified file to conflict copy %s: %w", conflictPath, normalizeSyncTreePathError(renameErr)))
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
				LocalMtime:   info.ModTime().UnixNano(),
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

	if err := e.syncTree.Remove(action.Path); err != nil {
		return e.failedOutcome(action, synctypes.ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, normalizeSyncTreePathError(err)))
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
