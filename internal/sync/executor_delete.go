package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	slashpath "path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// isDisposable returns true for bundled junk names that are safe to remove
// when configured junk filtering makes them non-content. Invalid OneDrive
// names are user-actionable observation issues, not disposable trash.
func isDisposable(name string) bool {
	return isBundledJunkName(name)
}

func (e *executor) isDisposable(name string) bool {
	return e.ignoreJunkFiles && isDisposable(name)
}

func (e *executor) findNonDisposable(dirPath string) string {
	return findNonDisposable(e.syncTree, dirPath, e.isDisposable)
}

func findNonDisposable(tree *synctree.Root, dirPath string, isDisposable func(string) bool) string {
	entries, err := tree.ReadDir(dirPath)
	if err != nil {
		return "."
	}

	for _, entry := range entries {
		if !isDisposable(entry.Name()) {
			return entry.Name()
		}

		if entry.IsDir() {
			if sub := findNonDisposable(tree, filepath.Join(dirPath, entry.Name()), isDisposable); sub != "" {
				if sub == "." {
					return entry.Name()
				}

				return entry.Name() + "/" + sub
			}
		}
	}

	return ""
}

// ExecuteLocalDelete removes a local file or folder with S4 safety:
// for files, verifies hash before delete; mismatch keeps the local file and
// recreates the remote copy instead of deleting newer content.
func (e *executor) ExecuteLocalDelete(ctx context.Context, action *Action) actionOutcome {
	if boundary, ok, err := e.symlinkBoundaryForPath(action.Path); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	} else if ok {
		cleanActionPath := slashpath.Clean(filepath.ToSlash(action.Path))
		if boundary != cleanActionPath {
			return e.failedOutcome(action, ActionLocalDelete,
				fmt.Errorf("refusing to delete %s through symlink boundary %s", action.Path, boundary))
		}

		return e.DeleteLocalSymlink(action, boundary)
	}

	info, err := e.syncTree.Stat(action.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return e.failedOutcomeWithFailure(
				action,
				ActionLocalDelete,
				stalePreconditionError("local delete source %s is already absent", action.Path),
				action.Path,
				permissionCapabilityLocalWrite,
			)
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

func (e *executor) DeleteLocalSymlink(action *Action, relPath string) actionOutcome {
	if err := e.syncTree.Remove(relPath); err != nil {
		return e.failedOutcome(
			action,
			ActionLocalDelete,
			fmt.Errorf("removing symlink %s: %w", action.Path, normalizeSyncTreePathError(err)),
		)
	}

	e.logger.Debug("deleted local symlink", slog.String("path", action.Path))

	return e.DeleteOutcome(action, ActionLocalDelete)
}

// DeleteLocalFolder removes an empty local directory. ReadDir is only the
// planner-facing blocker check: the final rooted RemoveEmptyDirNoFollow call
// rechecks emptiness, and the underlying rmdir fails closed if a child appears
// after that recheck.
func (e *executor) DeleteLocalFolder(action *Action, absPath string) actionOutcome {
	relPath, err := e.syncTree.Rel(absPath)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	preconditionErr := e.validateLocalDeleteFolderPrecondition(action, relPath)
	if preconditionErr != nil {
		return e.failedOutcomeWithFailure(action, ActionLocalDelete, preconditionErr, action.Path, permissionCapabilityLocalWrite)
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
			if !e.isDisposable(entry.Name()) {
				blockers = append(blockers, entry.Name())
			} else if entry.IsDir() {
				if nonDisp := e.findNonDisposable(entryPath); nonDisp != "" {
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

	if err := e.syncTree.RemoveEmptyDirNoFollow(relPath); err != nil {
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
func (e *executor) DeleteLocalFile(_ context.Context, action *Action, absPath string, info os.FileInfo) actionOutcome {
	baselineHash := ""
	baselineRemoteHash := ""

	if action.View != nil && action.View.Baseline != nil {
		baselineHash = action.View.Baseline.LocalHash
		baselineRemoteHash = action.View.Baseline.RemoteHash
	}

	// S4 safety: verify before delete. A baseline row can carry no local hash
	// -- the scanner records an empty hash when hashing a file fails -- and
	// skipping the check there would delete local content without ever asking
	// whether the user changed it, which is the destructive half of the case
	// S4 exists to prevent. Without a hash the baseline's size and mtime are
	// the only evidence left. They are too weak to prove two files equal, but
	// they are sound as a guard: any difference is proof the file changed, and
	// a destructive action may not run on no evidence at all.
	if baselineHash == "" {
		var baselineEntry *BaselineEntry
		if action.View != nil {
			baselineEntry = action.View.Baseline
		}

		if !localDeleteUnchangedWithoutHash(baselineEntry, info) {
			e.logger.Warn("local delete: no baseline hash and local file is unverified, keeping local file and requiring replan",
				slog.String("path", action.Path),
			)

			return e.failedOutcomeWithFailure(
				action,
				ActionLocalDelete,
				fmt.Errorf("%w: local delete unverifiable for %s (no baseline hash; remote=%s size=%d mtime=%d)",
					errActionPreconditionChanged,
					action.Path,
					baselineRemoteHash,
					info.Size(),
					info.ModTime().UnixNano(),
				),
				action.Path,
				permissionCapabilityLocalWrite,
			)
		}
	}

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
			return e.failedOutcomeWithFailure(
				action,
				ActionLocalDelete,
				fmt.Errorf("%w: local delete hash mismatch for %s (baseline=%s current=%s remote=%s mtime=%d)",
					errActionPreconditionChanged,
					action.Path,
					baselineHash,
					currentHash,
					baselineRemoteHash,
					info.ModTime().UnixNano(),
				),
				action.Path,
				permissionCapabilityLocalWrite,
			)
		}
	}

	if err := e.syncTree.Remove(action.Path); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, normalizeSyncTreePathError(err)))
	}

	e.logger.Debug("deleted local file", slog.String("path", action.Path))

	return e.DeleteOutcome(action, ActionLocalDelete)
}

// ExecuteRemoteDelete removes an item from OneDrive after checking the planned
// remote item still exists and matches the action preconditions.
func (e *executor) ExecuteRemoteDelete(ctx context.Context, action *Action) actionOutcome {
	driveID := e.resolveDriveID(action)

	sourceETag, err := e.remoteSourcePreconditionETag(ctx, driveID, action, "remote delete")
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteDelete,
			err,
			action.Path,
			permissionCapabilityRemoteRead,
		)
	}

	err = e.items.DeleteItemIfMatch(ctx, driveID, action.ItemID, sourceETag)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return e.failedOutcomeWithFailure(
				action,
				ActionRemoteDelete,
				stalePreconditionError("remote delete item %s disappeared", action.ItemID),
				action.Path,
				permissionCapabilityRemoteWrite,
			)
		}
		if errors.Is(err, graph.ErrPreconditionFailed) {
			return e.failedOutcomeWithFailure(
				action,
				ActionRemoteDelete,
				stalePreconditionError("remote delete item %s changed before mutation", action.ItemID),
				action.Path,
				permissionCapabilityRemoteWrite,
			)
		}

		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteDelete,
			fmt.Errorf("deleting remote %s: %w", action.Path, err),
			action.Path,
			inferFailureCapabilityFromError(err, permissionCapabilityUnknown, permissionCapabilityRemoteWrite),
		)
	}

	e.logger.Debug("deleted remote item", slog.String("path", action.Path), slog.String("item_id", action.ItemID))

	return e.DeleteOutcome(action, ActionRemoteDelete)
}

// DeleteOutcome builds a successful ActionOutcome for a delete action.
func (e *executor) DeleteOutcome(action *Action, actionType actionType) actionOutcome {
	return actionOutcome{
		Action:   actionType,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: actionItemType(action),
	}
}

// localDeleteUnchangedWithoutHash reports whether the baseline holds positive
// evidence that the file on disk is still the one that was synced, for rows
// that carry no local hash. Absence of evidence is not evidence of sameness,
// so an unknown size answers false rather than defaulting to deletable.
func localDeleteUnchangedWithoutHash(entry *BaselineEntry, info os.FileInfo) bool {
	if entry == nil || info == nil || !entry.LocalSizeKnown {
		return false
	}

	return entry.LocalSize == info.Size() && entry.LocalMtime == info.ModTime().UnixNano()
}
