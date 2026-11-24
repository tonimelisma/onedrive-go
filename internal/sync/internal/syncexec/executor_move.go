// The executor's moves: local renames and respellings, and remote moves.

package syncexec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ExecuteMove dispatches to local or remote move.
func (e *Executor) ExecuteMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	if action.Type == syncdomain.ActionLocalMove {
		return e.ExecuteLocalMove(ctx, action)
	}

	return e.ExecuteRemoteMove(ctx, action)
}

// ExecuteLocalMove renames a local file/folder.
func (e *Executor) ExecuteLocalMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	// Both endpoints are addressed by their spelling on disk (R-2.13.2): the
	// source and the destination's existing parents, and an existing
	// destination the move takes out of the way. A destination that does not
	// exist yet keeps the stored name.
	source := e.syncTree.Spelling(action.OldPath)
	dest := e.syncTree.Spelling(action.Path)

	// Both endpoints matter: an unchecked destination would still create
	// parent directories outside the sync root, and a replacing move takes
	// the file at the destination out of the folder.
	for _, endpoint := range []string{source, dest} {
		if err := e.validateNoSymlinkBoundary(endpoint, "move"); err != nil {
			return e.failedOutcomeWithFailure(
				action,
				syncdomain.ActionLocalMove,
				err,
				endpoint,
				syncdomain.PermissionCapabilityLocalWrite,
			)
		}
	}

	if err := e.validateLocalMovePrecondition(ctx, action, source, dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.OldPath,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	// The parent must already exist. Planning orders a move after whatever
	// creates or moves its target parent (D-054); fabricating a missing parent
	// here would hide an ordering bug and leave a stray folder for the next
	// pass to upload.
	if err := e.validateLocalMoveParentExists(dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	if err := e.clearLocalMoveDestination(ctx, action, dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	spellingOnly := isLocalSpellingOnlyMove(action.OldPath, action.Path)
	recordsIntent := !spellingOnly && movedItemType(action) == syncdomain.ItemTypeFile
	if recordsIntent {
		if err := e.moveIntents.RecordLocalMoveIntent(ctx, action.OldPath, action.Path); err != nil {
			return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove,
				fmt.Errorf("recording the move of %s: %w", action.OldPath, err),
				action.OldPath, syncdomain.PermissionCapabilityUnknown)
		}
	}
	oldNameLeft, err := e.renameLocal(source, dest, spellingOnly)
	if recordsIntent && !oldNameLeft {
		// The old name is gone, or the rename did not happen and the file is
		// still the one its baseline names there: either way nothing is
		// left for the intent to account for.
		e.clearLocalMoveIntent(ctx, action.OldPath)
	}
	if err != nil {
		if errors.Is(err, syncdomain.ErrActionPreconditionChanged) {
			// The rename found the destination taken: an entry took it after
			// the precondition checked it, or took the name a replacing move
			// had just freed. It is kept, and the move is replanned from what
			// is there now.
			return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove, err, action.Path, syncdomain.PermissionCapabilityLocalWrite)
		}
		return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove,
			fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, normalizeSyncTreePathError(err)),
			action.OldPath, syncdomain.PermissionCapabilityLocalWrite)
	}

	if err := e.reachCrashPoint(syncdomain.CrashPointLocalMoveDone); err != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove, err, action.Path, syncdomain.PermissionCapabilityUnknown)
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	o := e.moveOutcome(action)
	e.observeMovedLocalChangeTime(&o, dest)

	return o
}

// observeMovedLocalChangeTime records the change time a local rename gave the
// moved file. rename(2) keeps the inode, birth time, content and mtime the
// outcome carries over from the source baseline, but updates the inode change
// time on Linux filesystems; left at the source baseline's, the next scan saw
// metadata drift and published a second, metadata-only baseline update for
// every applied remote file move.
//
// The new ctime is taken only from the entry that kept the moved file's
// identity. An entry swapped in at dest after the rename keeps the old
// recorded facts, which the next scan re-reads instead of trusting. dest is
// the destination's rooted spelling on disk.
func (e *Executor) observeMovedLocalChangeTime(o *syncdomain.ActionOutcome, dest string) {
	if o.ItemType != syncdomain.ItemTypeFile || !o.LocalHasIdentity {
		return
	}
	info, err := e.syncTree.Lstat(dest)
	if err != nil {
		e.logger.Debug("moved file change time unavailable",
			slog.String("path", o.Path),
			slog.Any("error", err),
		)
		return
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return
	}
	recorded := synctree.FileIdentity{Device: o.LocalDevice, Inode: o.LocalInode, BirthTime: o.LocalBirthTime}
	if !synctree.SameIdentity(recorded, identity) {
		return
	}
	if ctime := syncdomain.FileChangeTime(info); ctime != 0 {
		o.LocalCtime = ctime
	}
}

// renameLocal renames within the sync tree, between rooted paths spelled as
// on disk, and never replaces an entry at newPath: every move starts with
// RenameNoReplace. A file moves without a window wherever the system has the
// no-replace flag or hard links, and a folder wherever it has the flag;
// elsewhere the entry takes a check of newPath and then a plain rename. A
// rename that finds newPath taken leaves the entry there, and its error
// matches ErrActionPreconditionChanged as well as synctree.ErrTargetExists.
// A rename that placed the entry but could not remove its old name
// (synctree.ErrSourceNotRemoved) is a move that happened and is logged.
//
// A spelling-only rename (spellingOnly: the stored paths differ only under
// OneDrive's name equivalence) whose no-replace rename finds newPath taken by
// the source itself, as on a volume that folds case, where both spellings
// name one entry, is then a plain rename. On a volume that tells the
// spellings apart, two hard links of one file named in the two spellings
// pass that check too, and the plain rename leaves both names. After a
// spelling-only rename, respellLocal makes sure the folder holds the new
// spelling.
//
// oldNameLeft reports a move that happened but left its old name: the system
// refused the no-replace flag, and the entry linked or copied to newPath
// could not be unlinked from oldPath. The caller keeps the move's intent
// then, so the old name is the move's known leftover and never uploaded as
// a new file (R-2.12.9).
func (e *Executor) renameLocal(oldPath, newPath string, spellingOnly bool) (oldNameLeft bool, err error) {
	err = e.syncTree.RenameNoReplace(oldPath, newPath)
	if spellingOnly && errors.Is(err, synctree.ErrTargetExists) && e.namesOneLocalEntry(oldPath, newPath) {
		err = e.syncTree.Rename(oldPath, newPath)
	}

	switch {
	case errors.Is(err, synctree.ErrSourceNotRemoved):
		e.logger.Warn("Moved "+newPath+", but could not remove the name "+oldPath+" it was moved from; "+
			"the next sync removes that name when it still holds the same file",
			slog.String("path", newPath), slog.String("old_path", oldPath), slog.Any("error", err))
		return true, nil
	case errors.Is(err, synctree.ErrTargetExists):
		return false, fmt.Errorf("%w: local move destination %s was taken before the move: %w",
			syncdomain.ErrActionPreconditionChanged, newPath, err)
	case err != nil:
		return false, err //nolint:wrapcheck // synctree names both paths
	case !spellingOnly:
		return false, nil
	}

	return false, e.respellLocal(newPath)
}

// clearLocalMoveIntent forgets a local move's intent once nothing is left
// for it, on a context detached from the action's and bounded like an
// outcome's commit: the rename already happened, or did not, whatever became
// of the action. Best effort: an intent left behind is settled at the next
// start, and while its old path still has a baseline row the planner sees
// that path as always.
func (e *Executor) clearLocalMoveIntent(ctx context.Context, oldPath string) {
	if err := RunDetachedStoreAccess(ctx, e.detachedStoreBound(), func(clearCtx context.Context) error {
		return e.moveIntents.ClearLocalMoveIntent(clearCtx, oldPath)
	}); err != nil {
		e.logger.Warn("Could not clear the record of a local move", slog.String("path", oldPath), slog.Any("error", err))
	}
}

// LocalMoveIntentStore is the executor's narrow view of the sync store's
// local move intents (store_local_move_intents.go).
type LocalMoveIntentStore interface {
	RecordLocalMoveIntent(ctx context.Context, oldPath, newPath string) error
	ClearLocalMoveIntent(ctx context.Context, oldPath string) error
}

// namesOneLocalEntry reports whether oldPath and newPath name one entry, as
// two spellings of a name do on a volume that folds case. A name that cannot
// be read names no entry, so the move is replanned instead.
func (e *Executor) namesOneLocalEntry(oldPath, newPath string) bool {
	oldInfo, err := e.syncTree.Lstat(oldPath)
	if err != nil {
		return false
	}
	newInfo, err := e.syncTree.Lstat(newPath)
	if err != nil {
		return false
	}

	return synctree.SameFile(oldInfo, newInfo)
}

// respellLocal gives the entry at newPath the spelling newPath when its
// folder still holds it under another one (a case-insensitive volume that
// takes a spelling-only rename as a no-op; Linux filesystems and APFS store
// the new spelling). It renames through a temporary sibling, both of whose
// steps never replace, named so that local observation always excludes it
// (isSpellingRenameArtifactName): a crash between the two renames must not
// leave a file that syncs under the temporary name (the engine's next start
// puts such a leftover back under its plain name). A failure of either step
// is the rename's own failure, even when the second finds newPath taken: the
// entry is then put back, or kept under the temporary name where it cannot
// be, and the failure says which.
func (e *Executor) respellLocal(newPath string) error {
	stored, err := e.storedNameSpelling(newPath)
	if err != nil || stored == filepath.Base(newPath) {
		return err
	}

	err = e.syncTree.RenameWithTemporarySibling(
		filepath.Join(filepath.Dir(newPath), stored), newPath,
		syncdomain.SpellingRenameArtifactPrefix+filepath.Base(newPath), syncdomain.SpellingRenameArtifactAttempts,
	)
	if errors.Is(err, synctree.ErrSourceNotRemoved) {
		// The entry has its new spelling; a name left behind is reported.
		e.logger.Warn("Renamed "+newPath+", but could not remove a name it was renamed from",
			slog.String("path", newPath), slog.Any("error", err))
		return nil
	}

	return err //nolint:wrapcheck // synctree names both paths
}

// storedNameSpelling returns the spelling the parent directory holds for the
// entry OneDrive would call path's name. It asks the OneDrive question of a
// local listing (namekey.Find): the entry spelled so, else the only one of a
// name OneDrive treats as the same; with none, path's own name. Two such
// entries and none spelled so are an error: renaming either onto the name
// would pick one of two files by listing order.
func (e *Executor) storedNameSpelling(path string) (string, error) {
	dir := filepath.Dir(path)
	entries, err := e.syncTree.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading %s after a spelling rename: %w", dir, normalizeSyncTreePathError(err))
	}

	base := filepath.Base(path)
	match := namekey.Find(entries, base, func(entry *fs.DirEntry) (string, bool) { return (*entry).Name(), true })
	switch {
	case match.Ambiguous:
		return "", fmt.Errorf("giving %s its spelling: %s holds more than one entry OneDrive treats as that name",
			path, dir)
	case !match.Found():
		return base, nil
	}

	return entries[match.Index].Name(), nil
}

// ExecuteRemoteMove renames/moves an item on OneDrive.
func (e *Executor) ExecuteRemoteMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	driveID := e.resolveDriveID(action)

	live, sourcePreconditionErr := e.remoteSourcePrecondition(ctx, driveID, action, "remote move")
	sourceETag := live.eTag
	if errors.Is(sourcePreconditionErr, errRemoteMoveAlreadyApplied) {
		// Already where the plan wants it (an earlier attempt or a parent
		// folder move got there first): record the move instead of
		// superseding work that has nothing left to do.
		e.logger.Debug("remote move already applied",
			slog.String("from", action.OldPath),
			slog.String("to", action.Path),
		)
		o := e.moveOutcome(action)
		o.ETag = sourceETag
		return o
	}
	if sourcePreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			sourcePreconditionErr,
			action.OldPath,
			syncdomain.PermissionCapabilityRemoteRead,
		)
	}
	newParentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionRemoteMove, err, action.OldPath, syncdomain.PermissionCapabilityUnknown)
	}
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, newParentID, action, "remote move")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			parentPreconditionErr,
			action.Path,
			syncdomain.PermissionCapabilityRemoteRead,
		)
	}

	newName := filepath.Base(action.Path)
	// No If-Match for a folder move: its eTag also changes when a child
	// changes, which does not make moving the folder unsafe, and a busy folder
	// would otherwise keep failing the move. Files keep the conditional.
	if plannedRemoteItemType(action) == syncdomain.ItemTypeFolder {
		sourceETag = ""
	}

	// The live modification time rides along: a rename on OneDrive otherwise
	// resets it to now, and the next comparison would see a change (D-150).
	item, err := e.items.MoveItemIfMatch(ctx, driveID, action.ItemID, newParentID, newName, graph.MoveOptions{
		IfMatch:      sourceETag,
		KeepModified: live.modified,
	})
	if err != nil {
		if errors.Is(err, graph.ErrPreconditionFailed) {
			return e.failedOutcomeWithFailure(
				action,
				syncdomain.ActionRemoteMove,
				stalePreconditionError("remote move item %s changed before mutation", action.ItemID),
				action.OldPath,
				syncdomain.PermissionCapabilityRemoteWrite,
			)
		}

		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err),
			action.OldPath,
			inferFailureCapabilityFromError(err, syncdomain.PermissionCapabilityUnknown, syncdomain.PermissionCapabilityRemoteWrite),
		)
	}

	if crashErr := e.reachCrashPoint(syncdomain.CrashPointRemoteMoveDone); crashErr != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionRemoteMove, crashErr, action.OldPath, syncdomain.PermissionCapabilityUnknown)
	}

	e.logger.Debug("remote move complete",
		slog.String("from", action.OldPath),
		slog.String("to", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	o := e.moveOutcome(action)
	o.ItemID = item.ID
	o.ETag = item.ETag

	return o
}
