// MoveStateDBs: an account email change's move of every state DB family,
// with its trash folders and upload sessions, under the store locks
// engine_locks.go takes.

package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// MoveStateDBs renames state stores, each with its whole file family, for an
// account email change (config.StateDBMover). It takes the store lock of
// every source and every target first and changes nothing unless it holds
// them all: a store an engine has open (ErrSyncStoreInUse) refuses every
// move, as does a target that already has a database. The locks are held
// until every rename is done, so no engine starts on either name meanwhile.
//
// A non-nil begin starts a new change. No journal names these moves yet, so
// whatever sidecars stand at a new name are a stranger's; they are deleted
// (SQLite would replay a log into the database moved beside it), and then
// begin records the change, before the first rename. A nil begin finishes a
// change a journal records: everything at a new name was moved there by it
// and is kept, and a database at both names is refused with the one to
// remove named.
//
// Before a store moves, a log it has is folded into its database where it
// can be (FoldStateDBLog), so the store is one file and moves in one rename;
// a store that cannot fold moves file by file, never deleting at its target.
//
// A source with no database is not a store: its sidecars are leftovers of a
// deleted one. There is nothing to move, and left behind they would sit
// under an email no account has any more, where no command looks, so they
// are deleted instead, after the moves. dataDir must be the data directory
// the engines use.
func MoveStateDBs(
	ctx context.Context, dataDir string, moves []config.StateDBMove, begin func() error, logger *slog.Logger,
) (err error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	release, err := lockStateDBMoves(ctx, dataDir, moves, logger)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release sync state lock: %w", releaseErr))
		}
	}()

	// A drive's shortcut children name their stores by the drive's mount ID,
	// which holds the email: they move with it, under their own locks too.
	children, err := childStateDBMoves(ctx, dataDir, moves, logger)
	if err != nil {
		return err
	}
	releaseChildren, err := lockStateDBMoves(ctx, dataDir, children, logger)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := releaseChildren(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release sync state lock: %w", releaseErr))
		}
	}()

	all := append(slices.Clone(moves), children...)
	pending, leftovers, err := splitStateDBMoves(all)
	if err != nil {
		return err
	}
	trashMoves, err := beginStateDBMoves(dataDir, all, pending, begin)
	if err != nil {
		return err
	}

	for _, move := range pending {
		foldStateDBLogBeforeMove(ctx, move, logger)
		if moveErr := syncstore.MoveStateDBFamily(move.From, move.To); moveErr != nil {
			return moveErr
		}
		logger.Info("moved sync state for an account email change",
			slog.String("from", move.From), slog.String("to", move.To))
	}

	if companionErr := moveStoreCompanions(ctx, dataDir, moves, all, trashMoves, logger); companionErr != nil {
		return companionErr
	}

	for _, dbPath := range leftovers {
		if removeErr := removeStateDBLeftovers(dbPath, logger); removeErr != nil {
			return removeErr
		}
	}

	return nil
}

// beginStateDBMoves checks every target of pending and of the trash folders
// of all and, for a new change (begin not nil), deletes the stranger's
// sidecars at the new names and records the change, before the first
// rename. It returns the trash folder moves.
func beginStateDBMoves(dataDir string, all, pending []config.StateDBMove, begin func() error) ([]trashDirMove, error) {
	trashMoves, err := trashDirMoves(dataDir, all)
	if err != nil {
		return nil, err
	}
	if checkErr := checkTrashDirMoveTargets(trashMoves, begin == nil); checkErr != nil {
		return nil, checkErr
	}
	if checkErr := checkStateDBMoveTargets(pending, begin == nil); checkErr != nil {
		return nil, checkErr
	}
	if begin == nil {
		return trashMoves, nil
	}
	for _, move := range pending {
		if removeErr := syncstore.RemoveStateDBSidecars(move.To); removeErr != nil {
			return nil, removeErr
		}
	}
	if beginErr := begin(); beginErr != nil {
		return nil, fmt.Errorf("record the account email change: %w", beginErr)
	}

	return trashMoves, nil
}

// childStateDBMoves are the moves of the stores of the shortcut children of
// every drive moves renames (a move with mount IDs): ChildMountID of the old
// mount ID to that of the new one, for each shortcut the drive's store
// records. The bindings are read from the drive's store where it is now, at
// its old name before the move or at its new one when a resumed change
// already moved it; the caller holds that store's lock. A move derived so is
// idempotent like every other: a resumed change derives the same moves, and
// the ones already done find their source gone.
func childStateDBMoves(
	ctx context.Context, dataDir string, moves []config.StateDBMove, logger *slog.Logger,
) ([]config.StateDBMove, error) {
	var children []config.StateDBMove
	for _, move := range moves {
		if move.FromMountID == "" || move.ToMountID == "" || move.FromMountID == move.ToMountID {
			continue
		}
		storePath, found, err := currentStateDBOfMove(move)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		bindings, err := syncstore.ShortcutRootBindingsAt(ctx, storePath,
			[]string{move.FromMountID, move.ToMountID}, logger)
		if err != nil {
			// A store that cannot be read names no shortcut this change can
			// know of: it moves as it is, and so does nothing else of it.
			// Refusing would leave the change unfinished for good, every
			// command failing on it, over sync state the next sync rebuilds.
			logger.Warn("Could not read the shared folders of a drive's sync state to move theirs with it; "+
				"their sync state stays under the old email, and the next sync rebuilds it",
				slog.String("db_path", storePath), slog.Any("error", err))
			continue
		}
		for _, binding := range bindings {
			from := config.MountStatePathForDataDir(dataDir, config.ChildMountID(move.FromMountID, binding))
			to := config.MountStatePathForDataDir(dataDir, config.ChildMountID(move.ToMountID, binding))
			if from != "" && to != "" && from != to {
				children = append(children, config.StateDBMove{
					From: from, To: to,
					FromMountID: config.ChildMountID(move.FromMountID, binding),
					ToMountID:   config.ChildMountID(move.ToMountID, binding),
				})
			}
		}
	}

	return children, nil
}

// currentStateDBOfMove is where move's store is now: at its old name, or at
// its new one once moved; found is false when neither holds a database.
func currentStateDBOfMove(move config.StateDBMove) (path string, found bool, err error) {
	for _, candidate := range []string{move.From, move.To} {
		exists, statErr := syncstore.StateDBFileExists(candidate)
		if statErr != nil {
			return "", false, fmt.Errorf("check sync state %s: %w", candidate, statErr)
		}
		if exists {
			return candidate, true, nil
		}
	}

	return "", false, nil
}

// renameMovedShortcutNamespaces gives the shortcut roots of every moved
// drive's store its new mount ID, once the store is at its new name.
func renameMovedShortcutNamespaces(ctx context.Context, moves []config.StateDBMove, logger *slog.Logger) error {
	for _, move := range moves {
		if move.FromMountID == "" || move.ToMountID == "" || move.FromMountID == move.ToMountID {
			continue
		}
		exists, err := syncstore.StateDBFileExists(move.To)
		if err != nil {
			return fmt.Errorf("check sync state %s: %w", move.To, err)
		}
		if !exists {
			continue
		}
		_, renameErr := syncstore.RenameShortcutRootNamespaceAt(ctx, move.To, move.FromMountID, move.ToMountID, logger)
		switch {
		case renameErr == nil:
		case errors.Is(renameErr, syncstore.ErrShortcutRootsUnreadable):
			// childStateDBMoves already said, once, that this store could not
			// be read; it names no shortcut to rename.
			logger.Debug("no shared folders to rename in a moved sync state that cannot be read",
				slog.String("db_path", move.To), slog.Any("error", renameErr))
		default:
			return fmt.Errorf("rename the shortcuts of %s for an account email change: %w", move.To, renameErr)
		}
	}

	return nil
}

// splitStateDBMoves separates the moves whose source has a database from the
// sources that hold only leftovers of a deleted store, or nothing.
func splitStateDBMoves(moves []config.StateDBMove) (pending []config.StateDBMove, leftovers []string, err error) {
	pending = make([]config.StateDBMove, 0, len(moves))
	for _, move := range moves {
		exists, statErr := syncstore.StateDBFileExists(move.From)
		switch {
		case statErr != nil:
			return nil, nil, fmt.Errorf("move sync state %s: %w", move.From, statErr)
		case exists:
			pending = append(pending, move)
		default:
			leftovers = append(leftovers, move.From)
		}
	}

	return pending, leftovers, nil
}

// checkStateDBMoveTargets refuses every move while one target already has a
// database, and names the file to remove: before a change is recorded the
// one at the new name was there first; while a recorded change is finished
// the drive's state is already at the new name, so the one at the old name
// was created afterwards.
func checkStateDBMoveTargets(pending []config.StateDBMove, isResumed bool) error {
	for _, move := range pending {
		exists, err := syncstore.StateDBFileExists(move.To)
		switch {
		case err != nil:
			return fmt.Errorf("check sync state target %s: %w", move.To, err)
		case !exists:
			continue
		case isResumed:
			return fmt.Errorf("finish moving sync state %s: %s exists too; the account email change already moved "+
				"the drive's sync state to %s, so %s was created after the account's email changed: "+
				"remove %s (and its -wal, -shm and -journal files) and run the command again",
				move.From, move.To, move.To, move.From, move.From)
		default:
			return fmt.Errorf("move sync state %s: %s already exists; it was there before the account's email "+
				"changed: remove %s (and its -wal, -shm and -journal files) and run the command again",
				move.From, move.To, move.To)
		}
	}

	return nil
}

// foldStateDBLogBeforeMove folds a source's log into its database when the
// source has a sidecar and nothing of its family is at the target yet: a
// move resumed after its log went ahead must keep that log, not fold the
// database without it. A failed fold is logged and the family moves as
// files, which loses nothing.
func foldStateDBLogBeforeMove(ctx context.Context, move config.StateDBMove, logger *slog.Logger) {
	// A file that cannot be checked is left to the move, which checks every
	// file again and reports it.
	family := config.StateDBFamily(move.From)
	hasSidecar := false
	for _, sidecar := range family[:len(family)-1] {
		if exists, err := syncstore.StateDBFileExists(sidecar); err == nil && exists {
			hasSidecar = true
			break
		}
	}
	if !hasSidecar {
		return
	}
	if targetFound, err := syncstore.StateDBFamilyExists(move.To); err != nil || targetFound {
		return
	}

	if err := syncstore.FoldStateDBLog(ctx, move.From); err != nil {
		logger.Warn("could not fold the sync state log into its database before an account email change "+
			"moves it; moving its files as they are",
			slog.String("db_path", move.From), slog.Any("error", err))
	}
}

// removeStateDBLeftovers deletes the sidecars at dbPath, a name that has no
// database. The caller holds the store lock.
func removeStateDBLeftovers(dbPath string, logger *slog.Logger) error {
	found, err := syncstore.StateDBFamilyExists(dbPath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := syncstore.RemoveStateDBFiles(dbPath); err != nil {
		return err
	}
	if logger != nil {
		logger.Info("removed sync state files left without their database, for an account email change",
			slog.String("db_path", dbPath))
	}

	return nil
}

// lockStateDBMoves takes the store lock of every source and target of moves,
// each path once (a second lock of one file in this process would refuse
// the first). On a refusal it lets go of what it took. The returned release
// drops every lock, in reverse.
func lockStateDBMoves(
	ctx context.Context, dataDir string, moves []config.StateDBMove, logger *slog.Logger,
) (func() error, error) {
	var held []func() error
	releaseAll := func() error {
		var releaseErr error
		for i := len(held) - 1; i >= 0; i-- {
			releaseErr = errors.Join(releaseErr, held[i]())
		}
		return releaseErr
	}

	// Each move names two stores.
	locked := make(map[string]struct{}, 2*len(moves))
	for _, move := range moves {
		for _, path := range []string{move.From, move.To} {
			if _, seen := locked[path]; seen {
				continue
			}
			release, err := LockDriveForOfflineChange(ctx, dataDir, "", path, logger)
			if err != nil {
				return nil, errors.Join(err, releaseAll())
			}
			locked[path] = struct{}{}
			held = append(held, release)
		}
	}

	return releaseAll, nil
}

// moveStoreCompanions moves what is named after the stores once the stores
// moved: the moved drives' shortcut records take their new mount IDs, every
// store's trash folder its new name, and every moved mount's upload sessions
// its new ID. Each step is idempotent, so a resumed change runs them all.
func moveStoreCompanions(
	ctx context.Context, dataDir string, drives, all []config.StateDBMove, trashMoves []trashDirMove, logger *slog.Logger,
) error {
	if err := renameMovedShortcutNamespaces(ctx, drives, logger); err != nil {
		return err
	}
	if err := moveTrashDirs(trashMoves, logger); err != nil {
		return err
	}
	removeEmptyChildTrashContainers(drives, logger)

	return renameUploadSessionMounts(dataDir, all, logger)
}

// trashDirMove is the move of one store's trash folder for an account email
// change: a store's trash is named by the store, and a shortcut child's lies
// under its drive's name (localtrash.ChildDir), so both move with the store.
type trashDirMove struct {
	from string
	to   string
}

// trashDirMoves are the trash folder moves of every store move.
func trashDirMoves(dataDir string, moves []config.StateDBMove) ([]trashDirMove, error) {
	trash := make([]trashDirMove, 0, len(moves))
	for _, move := range moves {
		from, err := storeTrashDir(dataDir, move.From, move.FromMountID)
		if err != nil {
			return nil, err
		}
		to, err := storeTrashDir(dataDir, move.To, move.ToMountID)
		if err != nil {
			return nil, err
		}
		if from != "" && to != "" && from != to {
			trash = append(trash, trashDirMove{from: from, to: to})
		}
	}

	return trash, nil
}

// storeTrashDir is the trash folder of the store at statePath that mountID
// runs on: a shortcut child's under its drive's trash, any other store's
// beside it (localtrash.DirForStatePath), as the engine's mount config has it.
func storeTrashDir(dataDir, statePath, mountID string) (string, error) {
	if !config.IsChildMountID(mountID) {
		return localtrash.DirForStatePath(statePath), nil
	}
	parent, err := driveid.NewCanonicalID(config.ChildMountParentID(mountID))
	if err != nil {
		return "", fmt.Errorf("find the drive of shortcut mount %s to move its trash: %w", mountID, err)
	}

	return localtrash.ChildDir(localtrash.DirForStatePath(config.DriveStatePathForDataDir(dataDir, parent)), statePath), nil
}

// checkTrashDirMoveTargets refuses, before anything moves, a trash folder
// that exists at both names: for a new change the new one was there before
// the email changed, and for a resumed one the old one was made after the
// change moved it; merging the two would mix two folders' entries.
func checkTrashDirMoveTargets(moves []trashDirMove, isResumed bool) error {
	for _, move := range moves {
		fromExists, err := pathExists(move.from)
		if err != nil || !fromExists {
			if err != nil {
				return fmt.Errorf("check trash folder %s: %w", move.from, err)
			}
			continue
		}
		toExists, err := pathExists(move.to)
		switch {
		case err != nil:
			return fmt.Errorf("check trash folder %s: %w", move.to, err)
		case !toExists:
			continue
		case isResumed:
			return fmt.Errorf("finish moving trash folder %s: %s exists too; the account email change already moved "+
				"the trash to %s, so %s was made after the account's email changed: restore or move what you need "+
				"out of %s, remove it, and run the command again", move.from, move.to, move.to, move.from, move.from)
		default:
			return fmt.Errorf("move trash folder %s: %s already exists; it was there before the account's email "+
				"changed: restore or move what you need out of %s, remove it, and run the command again",
				move.from, move.to, move.to)
		}
	}

	return nil
}

// moveTrashDirs renames each trash folder whose old name still exists to its
// new one; a folder already moved, or never made, needs nothing. The targets
// were checked before the change began, and engines are refused while it is
// unfinished, so nothing makes a new name meanwhile.
func moveTrashDirs(moves []trashDirMove, logger *slog.Logger) error {
	for _, move := range moves {
		exists, err := pathExists(move.from)
		if err != nil {
			return fmt.Errorf("check trash folder %s: %w", move.from, err)
		}
		if !exists {
			continue
		}
		if mkdirErr := localpath.MkdirAll(filepath.Dir(move.to), trashFolderPerm); mkdirErr != nil {
			return fmt.Errorf("make the folder of trash %s: %w", move.to, mkdirErr)
		}
		if renameErr := localpath.Rename(move.from, move.to); renameErr != nil {
			return fmt.Errorf("move trash folder %s to %s: %w", move.from, move.to, renameErr)
		}
		logger.Info("moved a trash folder for an account email change",
			slog.String("from", move.from), slog.String("to", move.to))
	}

	return nil
}

// trashFolderPerm is the mode of a trash folder's parent the move makes,
// that of every trash folder (localtrash).
const trashFolderPerm = 0o700

// removeEmptyChildTrashContainers removes the folder that held a moved
// drive's shortcut trash under the drive's old name, once its children moved
// out. Only an empty folder goes: whatever is still in it is left, said at
// Debug, since a folder that keeps something is no harm and nothing is lost.
func removeEmptyChildTrashContainers(moves []config.StateDBMove, logger *slog.Logger) {
	for _, move := range moves {
		if move.FromMountID == "" || config.IsChildMountID(move.FromMountID) {
			continue
		}
		container := localtrash.ChildrenDir(localtrash.DirForStatePath(move.From))
		if err := localpath.Remove(container); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Debug("left the old folder of a drive's shortcut trash", slog.String("path", container), slog.Any("error", err))
		}
	}
}

// renameUploadSessionMounts gives the upload session records of every moved
// mount its new mount ID (driveops.SessionStore.RenameMountID).
func renameUploadSessionMounts(dataDir string, moves []config.StateDBMove, logger *slog.Logger) error {
	sessions := driveops.NewSessionStore(dataDir, logger)
	for _, move := range moves {
		if move.FromMountID == "" || move.ToMountID == "" || move.FromMountID == move.ToMountID {
			continue
		}
		if _, err := sessions.RenameMountID(move.FromMountID, move.ToMountID); err != nil {
			return fmt.Errorf("rename the upload sessions of %s for an account email change: %w", move.FromMountID, err)
		}
	}

	return nil
}

// pathExists reports whether anything is at path, without following it.
func pathExists(path string) (bool, error) {
	_, err := localpath.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err //nolint:wrapcheck // the caller names the path
	}
}
