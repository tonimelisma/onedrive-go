package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// IsDisposable returns true for files that are safe to remove when they block
// a parent directory deletion. These are OS junk files, editor temps, and
// names invalid for OneDrive.
func IsDisposable(name string) bool {
	// OS junk files.
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
// for files, verifies hash before delete; mismatch keeps the local file and
// recreates the remote copy instead of deleting newer content.
func (e *Executor) ExecuteLocalDelete(ctx context.Context, action *Action) ActionOutcome {
	info, err := e.syncTree.Stat(action.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already gone — success.
			e.logger.Debug("local delete: already absent", slog.String("path", action.Path))
			return e.DeleteOutcome(action, ActionLocalDelete)
		}

		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	absPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	if info.IsDir() {
		return e.DeleteLocalFolder(action, absPath)
	}

	return e.DeleteLocalFile(ctx, action, absPath, info)
}

// DeleteLocalFolder removes an empty local directory.
// NOTE: There is an inherent TOCTOU race between ReadDir and Remove — a file
// could be created between the two calls. This is acceptable because the DAG
// guarantees child deletes complete before parent folder deletes, and new
// creations would be caught in the next sync pass.
func (e *Executor) DeleteLocalFolder(action *Action, absPath string) ActionOutcome {
	relPath, err := e.syncTree.Rel(absPath)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	entries, err := e.syncTree.ReadDir(relPath)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("reading dir %s: %w", action.Path, err))
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
			return e.failedOutcome(action, ActionLocalDelete,
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

	if err := e.syncTree.Remove(relPath); err != nil {
		return e.failedOutcome(
			action,
			ActionLocalDelete,
			fmt.Errorf("removing dir %s: %w", action.Path, normalizeSyncTreePathError(err)),
		)
	}

	e.logger.Debug("deleted local folder", slog.String("path", action.Path))

	return e.DeleteOutcome(action, ActionLocalDelete)
}

// DeleteLocalFile removes a file after verifying its hash matches baseline.
// Hash mismatch means the file changed after planning; the executor keeps the
// local file in place and returns a stale-precondition failure so the engine
// replans from current truth.
func (e *Executor) DeleteLocalFile(_ context.Context, action *Action, absPath string, info os.FileInfo) ActionOutcome {
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
			return e.failedOutcome(action, ActionLocalDelete,
				fmt.Errorf("hashing %s before delete: %w", action.Path, err))
		}

		if currentHash != baselineHash {
			e.logger.Warn("local delete: hash mismatch, keeping local file and requiring replan",
				slog.String("path", action.Path),
			)
			return e.failedOutcome(
				action,
				ActionLocalDelete,
				fmt.Errorf("%w: local delete hash mismatch for %s (baseline=%s current=%s remote=%s mtime=%d)",
					ErrActionPreconditionChanged,
					action.Path,
					baselineHash,
					currentHash,
					baselineRemoteHash,
					info.ModTime().UnixNano(),
				),
			)
		}
	}

	if err := e.syncTree.Remove(action.Path); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, normalizeSyncTreePathError(err)))
	}

	e.logger.Debug("deleted local file", slog.String("path", action.Path))

	return e.DeleteOutcome(action, ActionLocalDelete)
}

// ExecuteRemoteDelete removes an item from OneDrive. 404 is treated as
// success (item already deleted).
func (e *Executor) ExecuteRemoteDelete(ctx context.Context, action *Action) ActionOutcome {
	driveID := e.resolveDriveID(action)

	err := e.items.DeleteItem(ctx, driveID, action.ItemID)
	if err != nil {
		// 404 means already deleted — success.
		if errors.Is(err, graph.ErrNotFound) {
			e.logger.Debug("remote delete: already absent", slog.String("path", action.Path))
			return e.DeleteOutcome(action, ActionRemoteDelete)
		}

		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteDelete,
			fmt.Errorf("deleting remote %s: %w", action.Path, err),
			action.Path,
			inferFailureCapabilityFromError(err, PermissionCapabilityUnknown, PermissionCapabilityRemoteWrite),
		)
	}

	e.logger.Debug("deleted remote item", slog.String("path", action.Path), slog.String("item_id", action.ItemID))

	return e.DeleteOutcome(action, ActionRemoteDelete)
}

// DeleteOutcome builds a successful ActionOutcome for a delete action.
func (e *Executor) DeleteOutcome(action *Action, actionType ActionType) ActionOutcome {
	return ActionOutcome{
		Action:   actionType,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: actionItemType(action),
	}
}
