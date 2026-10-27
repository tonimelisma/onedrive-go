package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// isDisposable returns true for bundled junk names that are safe to remove
// when configured junk filtering makes them non-content. Invalid OneDrive
// names are user-actionable observation issues, not disposable trash.
func isDisposable(name string, isDir bool) bool {
	return isBundledJunkName(name) || (!isDir && isBundledJunkFileName(name))
}

func (e *executor) isDisposable(name string, isDir bool) bool {
	return e.ignoreJunkFiles && isDisposable(name, isDir)
}

func (e *executor) findNonDisposable(dirPath string) string {
	return findNonDisposable(e.syncTree, dirPath, e.isDisposable)
}

func findNonDisposable(tree *synctree.Root, dirPath string, isDisposable func(string, bool) bool) string {
	entries, err := tree.ReadDir(dirPath)
	if err != nil {
		return "."
	}

	for _, entry := range entries {
		if !isDisposable(entry.Name(), entry.IsDir()) {
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
				fmt.Errorf("won't delete %s: %s is a symbolic link, and sync never deletes through one", action.Path, boundary))
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

	return e.DeleteLocalFile(action)
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
			if !e.isDisposable(entry.Name(), entry.IsDir()) {
				blockers = append(blockers, entry.Name())
			} else if entry.IsDir() {
				if nonDisp := e.findNonDisposable(entryPath); nonDisp != "" {
					blockers = append(blockers, entry.Name()+"/"+nonDisp)
				}
			}
		}

		if len(blockers) > 0 {
			return e.failedOutcome(action, ActionLocalDelete, folderNotEmptyError(blockers))
		}

		// All entries are disposable — remove them before deleting the folder.
		for _, entry := range entries {
			entryPath := filepath.Join(relPath, entry.Name())
			if rmErr := e.syncTree.RemoveAll(entryPath); rmErr != nil {
				// Debug: the folder removal below then fails and carries
				// the user-visible reason.
				e.logger.Debug("failed to remove disposable file",
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

// DeleteLocalFile takes a file out of the sync folder after verifying that it
// still holds the synced content, and puts it in the drive's trash (P-4).
//
// Verification and removal are bound to one inode (D-010): the file is
// opened once, its content checked through that descriptor, and only that
// inode leaves the folder. A file replaced while it was being hashed (an
// editor's atomic save) is left in place and the delete is superseded. A
// mismatch means the file changed after planning; the executor keeps it and
// returns a stale-precondition failure so the engine replans from current
// truth.
func (e *executor) DeleteLocalFile(action *Action) actionOutcome {
	verify := func(f *os.File, info os.FileInfo) error {
		return e.verifyLocalDeleteContent(action, f, info)
	}

	if err := e.removeVerifiedLocalFile(action.Path, verify); err != nil {
		if errors.Is(err, errActionPreconditionChanged) || errors.Is(err, synctree.ErrDetachRaced) {
			// Debug: the returned precondition error supersedes the action
			// and the engine replans; nothing is asked of the user.
			e.logger.Debug("local delete: file changed, keeping it and requiring replan",
				slog.String("path", action.Path),
				slog.String("error", err.Error()),
			)
			if !errors.Is(err, errActionPreconditionChanged) {
				err = fmt.Errorf("%w: %w", errActionPreconditionChanged, err)
			}
			return e.failedOutcomeWithFailure(action, ActionLocalDelete, err, action.Path, permissionCapabilityLocalWrite)
		}
		if errors.Is(err, os.ErrNotExist) {
			return e.failedOutcomeWithFailure(
				action,
				ActionLocalDelete,
				stalePreconditionError("local delete source %s is already absent", action.Path),
				action.Path,
				permissionCapabilityLocalWrite,
			)
		}
		return e.failedOutcome(action, ActionLocalDelete, fmt.Errorf("removing %s: %w", action.Path, normalizeSyncTreePathError(err)))
	}

	e.logger.Debug("deleted local file", slog.String("path", action.Path))

	return e.DeleteOutcome(action, ActionLocalDelete)
}

// verifyLocalDeleteContent is S4, read through the descriptor that will be
// removed. A baseline row can carry no local hash -- the scanner records an
// empty hash when hashing a file fails -- and skipping the check there would
// delete local content without ever asking whether the user changed it.
// Without a hash the baseline's size and mtime are the only evidence left.
// They are too weak to prove two files equal, but sound as a guard: any
// difference is proof the file changed, and a destructive action may not run
// on no evidence at all.
func (e *executor) verifyLocalDeleteContent(action *Action, f *os.File, info os.FileInfo) error {
	var baselineEntry *BaselineEntry
	if action.View != nil {
		baselineEntry = action.View.Baseline
	}
	baselineHash := ""
	baselineRemoteHash := ""
	if baselineEntry != nil {
		baselineHash = baselineEntry.LocalHash
		baselineRemoteHash = baselineEntry.RemoteHash
	}

	if baselineHash == "" {
		if localDeleteUnchangedWithoutHash(baselineEntry, info) {
			return nil
		}
		return fmt.Errorf("%w: local delete unverifiable for %s (no baseline hash; remote=%s size=%d mtime=%d)",
			errActionPreconditionChanged,
			action.Path,
			baselineRemoteHash,
			info.Size(),
			info.ModTime().UnixNano(),
		)
	}

	currentHash, err := e.hashReader(f)
	if err != nil {
		return fmt.Errorf("hashing %s before delete: %w", action.Path, err)
	}
	if currentHash != baselineHash {
		return fmt.Errorf("%w: local delete hash mismatch for %s (baseline=%s current=%s remote=%s mtime=%d)",
			errActionPreconditionChanged,
			action.Path,
			baselineHash,
			currentHash,
			baselineRemoteHash,
			info.ModTime().UnixNano(),
		)
	}

	return nil
}

// removeVerifiedLocalFile takes the verified file at rel out of the sync
// folder: into the trash when the drive has one, otherwise removed.
func (e *executor) removeVerifiedLocalFile(rel string, verify synctree.VerifyFunc) error {
	if e.trash == nil {
		return e.syncTree.RemoveVerifiedRegularFile(rel, verify)
	}

	placement, err := e.trash.Reserve(e.syncTree.Path(), rel, localtrash.ReasonDeleted)
	if err != nil {
		return err
	}
	if err := e.syncTree.DetachRegularFile(rel, verify, placement.ItemPath); err != nil {
		if abandonErr := placement.Abandon(); abandonErr != nil {
			e.logger.Debug("could not remove unused trash entry",
				slog.String("path", rel), slog.String("error", abandonErr.Error()))
		}
		return err
	}
	e.commitTrashPlacement(rel, placement)

	return nil
}

// commitTrashPlacement records a placement whose file already left the sync
// folder. The delete itself has happened, so a failure here does not fail the
// action; it is reported because the uncommitted entry will be purged as
// crash debris instead of kept for the retention period.
func (e *executor) commitTrashPlacement(rel string, placement *localtrash.Placement) {
	if _, err := placement.Commit(); err != nil {
		e.logger.Warn("could not record a trashed file; it is in the trash folder but will not be listed",
			slog.String("path", rel),
			slog.String("trash_path", placement.ItemPath),
			slog.String("error", err.Error()),
		)
	}
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

// folderNotEmptyError is the user-facing reason a folder deleted on OneDrive
// cannot be removed locally: it still holds files that are not on OneDrive.
// The console prints it after the folder path, so it names at most a few
// blockers and says what to do instead of dumping the whole list.
func folderNotEmptyError(blockers []string) error {
	const maxNamed = 3
	named := blockers
	more := ""
	if len(blockers) > maxNamed {
		named = blockers[:maxNamed]
		more = fmt.Sprintf(" and %d more", len(blockers)-maxNamed)
	}

	return fmt.Errorf("folder still contains files that are not on OneDrive (%s%s); move or delete them to finish",
		strings.Join(named, ", "), more)
}
