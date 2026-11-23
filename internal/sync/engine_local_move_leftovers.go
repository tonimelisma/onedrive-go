package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// errLeftoverDiffers is a move's old name that no longer holds the moved
// file's content: what is there now is the user's.
var errLeftoverDiffers = errors.New("the old name holds other content than the moved file")

// settleLocalMoveLeftovers is the engine's start-of-run settling of the local
// moves whose intent outlived them (store_local_move_intents.go), before the
// first plan, beside the put-back of spelling leftovers. Only a live run
// calls it: a dry run changes nothing on disk.
func (e *Engine) settleLocalMoveLeftovers(ctx context.Context) {
	intents, err := e.baseline.ListLocalMoveIntents(ctx)
	if err != nil {
		e.logger.Warn("Could not read the record of interrupted local moves; the next sync checks them again",
			slog.Any("error", err))
		return
	}
	for i := range intents {
		if ctx.Err() != nil {
			return
		}
		settleLocalMoveLeftover(ctx, e.syncTree, e.baseline, intents[i], e.logger)
	}
}

// settleLocalMoveLeftover settles one local move's intent. The move recorded
// it before its rename and clears it once the old name is gone, so one found
// here is a move the process died in, or one whose old name could not be
// removed (synctree.ErrSourceNotRemoved, where the system refuses the
// no-replace flag and the file was linked or copied to its new name).
//
// The old name is removed only when it holds nothing the new name does not:
// the very same file (a second link, which has another name), or a copy
// whose content equals the new name's. Both are checked through the old
// name's open descriptor (RemoveVerifiedRegularFile), so the file removed is
// the one checked, and the new name must still be the file it was. Anything
// else at the old name is the user's: it is kept, said once with both paths,
// and the intent is cleared so the next plan syncs it as a file of its own.
// An old name already gone, or a new name that is not there (the process
// died before the rename), leaves nothing to settle. A removal that fails
// keeps the intent, so the old name stays hidden from the planner and the
// next start tries again. Nothing here fails the run.
func settleLocalMoveLeftover(
	ctx context.Context, tree *synctree.Root, store *syncstore.SyncStore, intent syncstore.LocalMoveIntent, logger *slog.Logger,
) {
	oldPath := tree.Spelling(filepath.FromSlash(intent.OldPath))
	newPath := tree.Spelling(filepath.FromSlash(intent.NewPath))
	attrs := []any{slog.String("path", intent.NewPath), slog.String("old_path", intent.OldPath)}

	oldInfo, oldErr := tree.Lstat(oldPath)
	newInfo, newErr := tree.Lstat(newPath)
	switch {
	case oldErr != nil && !errors.Is(oldErr, fs.ErrNotExist):
		logger.Debug("could not check the old name of an interrupted local move",
			append(attrs, slog.Any("error", oldErr))...)
		return
	case oldErr != nil, !oldInfo.Mode().IsRegular(), newErr != nil, !newInfo.Mode().IsRegular():
		clearLocalMoveIntentAtStart(ctx, store, intent, logger)
		return
	}

	newHash := ""
	if !synctree.SameFile(oldInfo, newInfo) {
		hash, hashErr := hashTreeFile(ctx, tree, newPath)
		if hashErr != nil {
			logger.Debug("could not read the new name of an interrupted local move",
				append(attrs, slog.Any("error", hashErr))...)
			return
		}
		newHash = hash
	}

	err := tree.RemoveVerifiedRegularFile(oldPath, func(f *os.File, info os.FileInfo) error {
		current, lstatErr := tree.Lstat(newPath)
		if lstatErr != nil || !synctree.SameFile(current, newInfo) {
			return fmt.Errorf("%s changed while it was checked", intent.NewPath)
		}
		if synctree.SameFile(info, current) {
			if !hasAnotherName(info) {
				return fmt.Errorf("%s has no other name", intent.OldPath)
			}
			return nil
		}
		hash, hashErr := driveops.ComputeQuickXorHashFromReaderContext(ctx, f)
		if hashErr != nil {
			return fmt.Errorf("reading %s: %w", intent.OldPath, hashErr)
		}
		if newHash == "" || hash != newHash {
			return errLeftoverDiffers
		}
		return nil
	})
	switch {
	case err == nil:
		logger.Info("removed "+intent.OldPath+", the name a local move to "+intent.NewPath+" could not remove", attrs...)
		clearLocalMoveIntentAtStart(ctx, store, intent, logger)
	case errors.Is(err, errLeftoverDiffers):
		logger.Warn("Kept "+intent.OldPath+": a local move to "+intent.NewPath+" left this name, and it now "+
			"holds other content; it syncs as a file of its own", attrs...)
		clearLocalMoveIntentAtStart(ctx, store, intent, logger)
	default:
		logger.Warn("Could not remove "+intent.OldPath+", the name a local move to "+intent.NewPath+
			" left behind; the next sync tries again", append(attrs, slog.Any("error", err))...)
	}
}

// hashTreeFile is the QuickXorHash of the file at rel in tree.
func hashTreeFile(ctx context.Context, tree *synctree.Root, rel string) (hash string, err error) {
	f, err := tree.Open(rel)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", rel, closeErr)
		}
	}()

	hash, err = driveops.ComputeQuickXorHashFromReaderContext(ctx, f)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", rel, err)
	}

	return hash, nil
}

// clearLocalMoveIntentAtStart forgets a settled intent; one that cannot be
// cleared is settled again at the next start, the same way.
func clearLocalMoveIntentAtStart(ctx context.Context, store *syncstore.SyncStore, intent syncstore.LocalMoveIntent, logger *slog.Logger) {
	if err := store.ClearLocalMoveIntent(ctx, intent.OldPath); err != nil {
		logger.Debug("could not clear the record of a settled local move",
			slog.String("old_path", intent.OldPath), slog.Any("error", err))
	}
}
