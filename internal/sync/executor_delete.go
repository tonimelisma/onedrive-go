package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// executeLocalDelete removes a local file or folder with S4 safety:
// for files, verifies hash before delete; mismatch triggers conflict copy.
func (e *Executor) executeLocalDelete(_ context.Context, action *Action) Outcome {
	absPath := filepath.Join(e.syncRoot, action.Path)

	info, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		// Already gone — success.
		e.logger.Debug("local delete: already absent", slog.String("path", action.Path))
		return e.deleteOutcome(action, ActionLocalDelete)
	}

	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("stat %s: %w", action.Path, err))
	}

	if info.IsDir() {
		return e.deleteLocalFolder(action, absPath)
	}

	return e.deleteLocalFile(action, absPath)
}

// deleteLocalFolder removes an empty local directory.
func (e *Executor) deleteLocalFolder(action *Action, absPath string) Outcome {
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("reading dir %s: %w", action.Path, err))
	}

	if len(entries) > 0 {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("directory %s is not empty (%d entries)", action.Path, len(entries)))
	}

	if err := os.Remove(absPath); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("removing dir %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted local folder", slog.String("path", action.Path))

	return e.deleteOutcome(action, ActionLocalDelete)
}

// deleteLocalFile removes a file after verifying its hash matches baseline.
// Hash mismatch means the file was modified since the planner ran — rename
// to conflict copy instead of deleting.
func (e *Executor) deleteLocalFile(action *Action, absPath string) Outcome {
	baselineHash := ""
	if action.View != nil && action.View.Baseline != nil {
		baselineHash = action.View.Baseline.LocalHash
	}

	// S4 safety: verify hash before delete.
	if baselineHash != "" {
		currentHash, err := e.hashFunc(absPath)
		if err != nil {
			return e.failedOutcome(action, ActionLocalDelete,
				fmt.Errorf("hashing %s before delete: %w", action.Path, err))
		}

		if currentHash != baselineHash {
			// File was modified — save as conflict copy instead of deleting.
			conflictPath := conflictCopyPath(absPath, e.nowFunc())
			if renameErr := os.Rename(absPath, conflictPath); renameErr != nil {
				return e.failedOutcome(action, ActionLocalDelete,
					fmt.Errorf("renaming modified file to conflict copy %s: %w", conflictPath, renameErr))
			}

			e.logger.Warn("local delete: hash mismatch, saved conflict copy",
				slog.String("path", action.Path),
				slog.String("conflict_copy", filepath.Base(conflictPath)),
			)

			// Still mark as success — the baseline entry should be removed.
			return e.deleteOutcome(action, ActionLocalDelete)
		}
	}

	if err := os.Remove(absPath); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted local file", slog.String("path", action.Path))

	return e.deleteOutcome(action, ActionLocalDelete)
}

// executeRemoteDelete removes an item from OneDrive. 404 is treated as
// success (item already deleted).
func (e *Executor) executeRemoteDelete(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	err := e.withRetry(ctx, "remote delete "+action.Path, func() error {
		return e.items.DeleteItem(ctx, driveID, action.ItemID)
	})
	if err != nil {
		// 404 means already deleted — success.
		if errors.Is(err, graph.ErrNotFound) {
			e.logger.Debug("remote delete: already absent", slog.String("path", action.Path))
			return e.deleteOutcome(action, ActionRemoteDelete)
		}

		return e.failedOutcome(action, ActionRemoteDelete, fmt.Errorf("deleting remote %s: %w", action.Path, err))
	}

	e.logger.Debug("deleted remote item", slog.String("path", action.Path), slog.String("item_id", action.ItemID))

	return e.deleteOutcome(action, ActionRemoteDelete)
}

// deleteOutcome builds a successful Outcome for a delete action.
func (e *Executor) deleteOutcome(action *Action, actionType ActionType) Outcome {
	return Outcome{
		Action:  actionType,
		Success: true,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}
