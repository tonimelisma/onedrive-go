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

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
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
	// The entry is removed by its spelling on disk (R-2.13.2).
	local := e.syncTree.Spelling(action.Path)
	if boundary, ok, err := e.symlinkBoundaryForPath(local); err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	} else if ok {
		cleanLocalPath := slashpath.Clean(filepath.ToSlash(local))
		if boundary != cleanLocalPath {
			return e.failedOutcome(action, ActionLocalDelete,
				fmt.Errorf("won't delete %s: %s is a symbolic link, and sync never deletes through one", action.Path, boundary))
		}

		return e.DeleteLocalSymlink(action, boundary)
	}

	info, err := e.syncTree.Stat(local)
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

	absPath, err := e.syncTree.Abs(local)
	if err != nil {
		return e.failedOutcome(action, ActionLocalDelete, normalizeSyncTreePathError(err))
	}

	if info.IsDir() {
		return e.DeleteLocalFolder(action, absPath)
	}

	return e.DeleteLocalFile(action, local)
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
			if e.allBlockersIgnored(relPath, blockers) {
				// Everything left is content sync deliberately does not own
				// (an ignored dir, a dotfile under ignore_dotfiles). Failing
				// here retried forever (D-012). The folder's synced identity
				// is gone on OneDrive, so drop it from the baseline and keep
				// the folder with what it still holds; the next pass sees a
				// local-only folder and re-creates it, empty, online.
				e.logger.Debug("local delete: folder holds only ignored content, keeping it and forgetting the synced folder",
					slog.String("path", action.Path),
					slog.Int("ignored_entries", len(blockers)),
				)
				return e.DeleteOutcome(action, ActionLocalDelete)
			}
			return e.failedOutcome(action, ActionLocalDelete, folderNotEmptyError(blockers))
		}
	}

	disposableErr := e.removeDisposableChildren(relPath, entries)
	if err := e.syncTree.RemoveEmptyDirNoFollow(relPath); err != nil {
		removeErr := fmt.Errorf("removing dir %s: %w", action.Path, normalizeSyncTreePathError(err))
		if disposableErr != nil {
			// The leftover junk is why the folder is not empty; say so.
			removeErr = fmt.Errorf("%w (after failing to remove junk inside it: %w)", removeErr, disposableErr)
		}
		return e.failedOutcome(action, ActionLocalDelete, removeErr)
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
// truth. local is the file's rooted spelling on disk.
func (e *executor) DeleteLocalFile(action *Action, local string) actionOutcome {
	verify := func(f *os.File, info os.FileInfo) error {
		return e.verifyLocalDeleteContent(action, f, info)
	}

	if err := e.removeVerifiedLocalFile(local, verify); err != nil {
		if errors.Is(err, errActionPreconditionChanged) || errors.Is(err, synctree.ErrDetachRaced) {
			// Debug: the returned precondition error supersedes the action
			// and the engine replans; nothing is asked of the user.
			e.logger.Debug("local delete: file changed, keeping it and requiring replan",
				slog.String("path", action.Path),
				slog.Any("error", err),
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
		if err := e.syncTree.RemoveVerifiedRegularFile(rel, verify); err != nil {
			return fmt.Errorf("removing verified file: %w", err)
		}
		return nil
	}

	placement, err := e.trash.Reserve(e.syncTree.Path(), rel, localtrash.ReasonDeleted)
	if err != nil {
		return fmt.Errorf("reserving a trash entry: %w", err)
	}
	if err := e.syncTree.DetachRegularFile(rel, verify, placement.ItemPath); err != nil {
		if abandonErr := placement.Abandon(); abandonErr != nil {
			e.logger.Debug("could not remove unused trash entry",
				slog.String("path", rel), slog.Any("error", abandonErr))
		}
		return fmt.Errorf("moving to the trash: %w", err)
	}
	e.commitTrashPlacement(rel, placement)

	return nil
}

// commitTrashPlacement records a placement whose file already left the sync
// folder. The delete itself has happened, so a failure here does not fail the
// action; the entry keeps its pending record, which lists and restores and is
// never purged automatically.
func (e *executor) commitTrashPlacement(rel string, placement *localtrash.Placement) {
	if _, err := placement.Commit(); err != nil {
		e.logger.Warn("could not finish recording a trashed file; it stays listed as interrupted and can be restored",
			slog.String("path", rel),
			slog.String("trash_path", placement.ItemPath),
			slog.Any("error", err),
		)
	}
}

// ExecuteRemoteDelete removes an item from OneDrive after checking the planned
// remote item still exists and matches the action preconditions.
func (e *executor) ExecuteRemoteDelete(ctx context.Context, action *Action) actionOutcome {
	driveID := e.resolveDriveID(action)

	if err := e.validateRemoteDeleteLocalAbsence(action); err != nil {
		return e.failedOutcomeWithFailure(action, ActionRemoteDelete, err, action.Path, permissionCapabilityLocalWrite)
	}

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
	if childErr := e.validateRemoteFolderDeleteChildren(ctx, driveID, action); childErr != nil {
		return e.failedOutcomeWithFailure(action, ActionRemoteDelete, childErr, action.Path, permissionCapabilityRemoteRead)
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

// removeDisposableChildren removes the junk entries left in a folder about to
// be deleted (every entry is disposable when this runs). A failure is logged
// at Warn and returned, so the folder-removal error that follows names it.
func (e *executor) removeDisposableChildren(relPath string, entries []os.DirEntry) error {
	var errs []error
	for _, entry := range entries {
		entryPath := filepath.Join(relPath, entry.Name())
		if rmErr := e.syncTree.RemoveAll(entryPath); rmErr != nil {
			rmErr = normalizeSyncTreePathError(rmErr)
			e.logger.Warn("Could not remove junk file from a deleted folder",
				slog.String("path", entryPath),
				slog.Any("error", rmErr),
			)
			errs = append(errs, fmt.Errorf("removing %s: %w", entryPath, rmErr))
		}
	}
	return errors.Join(errs...)
}

// allBlockersIgnored reports whether every child blocking a folder delete is
// hidden by the content filter, so sync never synced it and cannot own it.
func (e *executor) allBlockersIgnored(relPath string, blockers []string) bool {
	for _, blocker := range blockers {
		childPath := filepath.ToSlash(filepath.Join(relPath, blocker))
		info, err := e.syncTree.Lstat(childPath)
		if err != nil {
			return false
		}
		itemType := ItemTypeFile
		if info.IsDir() {
			itemType = ItemTypeFolder
		}
		if e.contentFilter.Visible(childPath, itemType) {
			return false
		}
	}

	return true
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

// validateRemoteDeleteLocalAbsence is the remote-delete mirror of S4: a
// delete on OneDrive is planned from local absence, so the absence must still
// hold when it runs (D-011). The item may have come back (a restore, a
// lone MOVED_FROM that was really a move within the folder), and the sync
// folder itself must still be the one the state belongs to, or "absent"
// means nothing.
func (e *executor) validateRemoteDeleteLocalAbsence(action *Action) error {
	if err := e.syncRootIdentity.Validate(e.syncTree); err != nil {
		return err
	}

	// Absence is judged by the spelling on disk: on a byte-exact filesystem
	// an NFD-named file that came back is missing only under its stored NFC
	// spelling (R-2.13.2).
	local := e.syncTree.Spelling(action.Path)
	info, err := e.syncTree.Lstat(local)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || e.hasRegularFileAncestor(local) {
			// Below a file nothing can exist: the folder that held this
			// child was replaced locally by a file (a type change), and the
			// child is as absent as it gets.
			return nil
		}
		return normalizeSyncTreePathError(err)
	}

	// A type-change replacement deletes the old item while the new one, of
	// the other kind, already sits at the path; that is not a reappearance.
	isDir := info.IsDir()
	if (deletedItemType(action) == ItemTypeFolder) != isDir {
		return nil
	}
	// On a case-insensitive volume, Lstat also finds a differently-cased
	// name, which is the rename this delete belongs to.
	if !e.exactLocalNameExists(local) {
		return nil
	}

	return stalePreconditionError("remote delete source %s exists locally again", action.Path)
}

// hasRegularFileAncestor reports whether a parent component of relPath is a
// regular file (not a link), checked from the sync root down.
func (e *executor) hasRegularFileAncestor(relPath string) bool {
	parts := strings.Split(filepath.Clean(relPath), string(filepath.Separator))
	current := ""
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := e.syncTree.Lstat(current)
		if err != nil {
			return false
		}
		if info.Mode().IsRegular() {
			return true
		}
		if !info.IsDir() {
			return false
		}
	}

	return false
}

func (e *executor) exactLocalNameExists(relPath string) bool {
	entries, err := e.syncTree.ReadDir(filepath.Dir(relPath))
	if err != nil {
		// Unknown: keep the conservative answer that the path is occupied.
		return true
	}
	want := nfcNormalize(filepath.Base(relPath))
	for _, entry := range entries {
		if nfcNormalize(entry.Name()) == want {
			return true
		}
	}
	return false
}

// remoteFolderDeleteListLimit bounds the live listing a folder delete makes
// before it may run. Descendants the plan deleted are already gone by then
// (child deletes run first), so the listing is normally one empty page.
const remoteFolderDeleteListLimit = 10000

// validateRemoteFolderDeleteChildren refuses a recursive folder delete that
// would take content the planner never saw (D-006). A DELETE of a folder
// removes everything beneath it, and the folder precondition deliberately
// skips the folder's eTag (it churns with every descendant change). A child
// created on OneDrive after observation, or hidden by delta lag, was deleted
// with no notice. Every live descendant must be a synced item whose content
// is unchanged; anything else supersedes the delete so the next pass, which
// observes it, plans from the truth. The If-Match on the folder eTag read
// before this listing still turns a child added after it into a 412.
func (e *executor) validateRemoteFolderDeleteChildren(ctx context.Context, driveID driveid.ID, action *Action) error {
	if action.ItemID == "" || deletedItemType(action) != ItemTypeFolder {
		return nil
	}

	pending := []string{action.ItemID}
	seen := 0
	for len(pending) > 0 {
		folderID := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		children, err := e.items.ListChildren(ctx, driveID, folderID)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				return stalePreconditionError("remote delete folder %s disappeared while listing", action.Path)
			}
			return fmt.Errorf("listing %s before deleting it on OneDrive: %w", action.Path, err)
		}
		for i := range children {
			seen++
			if seen > remoteFolderDeleteListLimit {
				// Not stale: replanning would list the same folder again on
				// every pass. A durable issue asks the user instead.
				return fmt.Errorf("%w: %s holds more than %d items", errRemoteFolderTooLargeToVerify,
					action.Path, remoteFolderDeleteListLimit)
			}
			child := &children[i]
			if err := e.validateLiveFolderDeleteChild(action, child); err != nil {
				return err
			}
			if child.IsFolder {
				pending = append(pending, child.ID)
			}
		}
	}

	return nil
}

func (e *executor) validateLiveFolderDeleteChild(action *Action, child *graph.Item) error {
	var known *BaselineEntry
	if e.baseline != nil {
		known, _ = e.baseline.GetByID(child.ID)
	}
	if known == nil {
		return stalePreconditionError("remote delete folder %s holds %q, which sync has not seen", action.Path, child.Name)
	}
	if child.IsFolder || known.ItemType != ItemTypeFile {
		// Folder eTags churn with every descendant change; the descendants
		// themselves are checked as the walk reaches them.
		return nil
	}
	if liveFileChangedSinceSync(child, known) {
		return stalePreconditionError("remote delete folder %s holds %s, changed on OneDrive since it was synced", action.Path, known.Path)
	}

	return nil
}

// liveFileChangedSinceSync compares a live file with its synced record by
// content hash, eTag and size. The baseline keeps no cTag, so a content change
// that kept the eTag and hash (none is known) is the remaining blind spot.
func liveFileChangedSinceSync(live *graph.Item, known *BaselineEntry) bool {
	if liveHash := driveops.SelectHash(live); liveHash != "" && known.RemoteHash != "" && liveHash != known.RemoteHash {
		return true
	}
	if live.ETag != "" && known.ETag != "" && live.ETag != known.ETag {
		return true
	}
	return known.RemoteSizeKnown && live.Size != known.RemoteSize
}

// errRemoteFolderTooLargeToVerify reports a folder delete whose live content
// is too large to list before deleting it recursively. It becomes a durable
// issue rather than a stale precondition that would replan and relist the
// folder on every pass.
var errRemoteFolderTooLargeToVerify = errors.New("folder is too large to verify before deleting it on OneDrive")
