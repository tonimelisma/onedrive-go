package sync

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ErrSyncRootInUse means another onedrive-go engine, in this process or
// another, already holds the sync root. The control-socket owner lock only
// covers one control socket, so two configurations sharing a data directory
// could each start an engine on one folder and fight over it (D-083).
var ErrSyncRootInUse = errors.New("another onedrive-go sync is already syncing this folder")

// ErrSyncStoreInUse means another onedrive-go engine, in this process or
// another, has this state store open, or an offline change to it is running.
// The sync-root lock cannot see this case: two configurations sharing a data
// directory can give one drive two different sync folders, and so one store.
var ErrSyncStoreInUse = errors.New("another onedrive-go sync is already using this sync state")

// ErrEmailChangeUnfinished means the drive's state store is named by the
// journal of an account email change that was cut short: the store may be at
// its old name, at its new one, or split between them. An engine never
// finishes the change and never waits for it; it refuses to start, and the
// next onedrive-go command that uses an account finishes the change at its
// entry (only help and the completion scripts skip that).
var ErrEmailChangeUnfinished = errors.New(
	"an account email change is not finished yet; run any onedrive-go command, such as " +
		"'onedrive-go status', to finish it")

// engineLocksDir is the data-directory subfolder that holds the lock files.
const engineLocksDir = "locks"

// engineLockPerm and engineLocksDirPerm keep lock files private.
const (
	engineLockPerm     = 0o600
	engineLocksDirPerm = 0o700
)

// The lock order is fixed, for engines and offline changes alike: the
// sync-root lock, then the store lock, then the store opens; both are
// released in reverse, after the store has closed. Every attempt is
// non-blocking (LOCK_NB): a held lock is refused at once, so no attempt ever
// waits and no order can deadlock. Keeping one order still matters: whoever
// comes second is refused on the same lock, with the same message.

// dataDirLock is an exclusive flock on a regular file in the data
// directory's lock folder, held for an engine's lifetime or for one offline
// change. The lock file lives in the data directory, not the sync folder,
// because flock on the folder itself fails on Linux NFS (EBADF on a
// read-only directory descriptor) and on some FUSE filesystems (ENOSYS),
// which made the engine refuse folders it used to sync. Lock files are never
// removed: unlinking one while another process may hold a descriptor to it
// would split the lock in two.
type dataDirLock struct {
	file  *os.File
	flock func(fd int, how int) error
}

// lockTarget is what one lock file guards, for its messages.
type lockTarget struct {
	// inUse is the sentinel a refusal wraps.
	inUse error
	// noun names the guarded thing: "sync folder" or "sync state".
	noun string
	// name is the folder or the store path.
	name string
}

// engineLocks are the two locks an engine holds for its lifetime, taken in
// the fixed order. A nil member holds nothing.
type engineLocks struct {
	root  *dataDirLock
	store *dataDirLock
}

// engineLockDataDir is where an engine's locks live: its data dir, or the
// state DB's folder (which is inside it) when no data dir is set.
func engineLockDataDir(cfg *engineInputs) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}

	return filepath.Dir(cfg.DBPath)
}

// acquireEngineLocks takes an engine's locks in order: the folder it syncs
// (keyed by inode, so every path to the folder contends), then its state
// store (keyed by the store's path, so every engine that names the store
// contends, whatever folder it syncs). It must run before the store opens:
// a refused engine never touches the store. A store lock refused after the
// folder lock was taken lets the folder go again.
//
// Holding both, it refuses a store an unfinished account email change moves
// (ErrEmailChangeUnfinished): such a store may be at either name or split
// between them, and the engine neither waits for the change nor finishes
// it. The refusal lets both locks go.
func acquireEngineLocks(cfg *engineInputs) (engineLocks, error) {
	dataDir := engineLockDataDir(cfg)
	root, err := acquireSyncRootLock(dataDir, cfg.SyncRoot, cfg.Logger, cfg.lockFlock)
	if err != nil {
		return engineLocks{}, err
	}
	store, err := acquireSyncStoreLock(dataDir, cfg.DBPath, cfg.Logger, cfg.lockFlock)
	if err != nil {
		return engineLocks{}, errors.Join(err, root.release())
	}
	locks := engineLocks{root: root, store: store}
	if refuseErr := refuseStoreOfUnfinishedEmailChange(dataDir, cfg.DBPath); refuseErr != nil {
		return engineLocks{}, errors.Join(refuseErr, locks.release())
	}

	return locks, nil
}

// release drops the store lock, then the folder lock.
func (l engineLocks) release() error {
	storeErr := l.store.release()
	rootErr := l.root.release()

	return errors.Join(storeErr, rootErr)
}

// syncRootLockPath names the lock file for root inside dataDir: its device
// and inode.
func syncRootLockPath(dataDir, root string) (string, error) {
	info, err := localpath.Stat(root)
	if err != nil {
		return "", fmt.Errorf("stat sync folder for locking: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("stat sync folder for locking: no device and inode for %s", root)
	}

	name := fmt.Sprintf("root-%d-%d.lock", statDeviceNumber(stat.Dev), stat.Ino)

	return filepath.Join(dataDir, engineLocksDir, name), nil
}

// syncStoreLockPath names the lock file of the state store at dbPath inside
// dataDir: a digest of the store's absolute path with its folders' symlinks
// resolved, so every path that names one store names one lock. The name
// does not depend on the store existing or on its inode, so a store that is
// deleted and recreated (reset, purge) keeps its lock.
func syncStoreLockPath(dataDir, dbPath string) (string, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve sync state path for locking: %w", err)
	}
	dir := resolveExistingAncestors(filepath.Dir(abs))
	sum := sha256.Sum256([]byte(filepath.Join(dir, filepath.Base(abs))))
	name := "store-" + base64.RawURLEncoding.EncodeToString(sum[:]) + ".lock"

	return filepath.Join(dataDir, engineLocksDir, name), nil
}

// resolveExistingAncestors resolves the symlinks of the deepest folder of
// path that can be resolved and keeps the rest as written. A folder that
// does not exist yet (a store about to be created) has no symlinks to
// resolve, and resolving what does exist gives every caller the same answer
// whether or not the rest exists yet.
func resolveExistingAncestors(path string) string {
	var rest []string
	for dir := path; ; dir = filepath.Dir(dir) {
		if resolved, err := localpath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
	}
}

// statDeviceNumber normalizes Stat_t.Dev, which is uint64 on Linux and
// int32 on macOS, so the lock name has one form on both.
func statDeviceNumber[T ~int32 | ~uint64](dev T) uint64 {
	return uint64(dev)
}

// isUnsupportedLockErrno reports flock failures that mean "this filesystem
// cannot lock", as opposed to "someone holds the lock".
func isUnsupportedLockErrno(err error) bool {
	for _, errno := range []syscall.Errno{
		syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK, syscall.EINVAL, syscall.EBADF, syscall.ENOSYS,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	return false
}

// acquireSyncRootLock takes the lock of the folder root. flock is the
// syscall (a test seam; nil is syscall.Flock).
func acquireSyncRootLock(
	dataDir, root string, logger *slog.Logger, flock func(fd int, how int) error,
) (*dataDirLock, error) {
	path, err := syncRootLockPath(dataDir, root)
	if err != nil {
		return nil, err
	}

	return lockDataDirFile(path, lockTarget{inUse: ErrSyncRootInUse, noun: "sync folder", name: root}, logger, flock)
}

// acquireSyncStoreLock takes the lock of the state store at dbPath. flock
// is the syscall (a test seam; nil is syscall.Flock).
func acquireSyncStoreLock(
	dataDir, dbPath string, logger *slog.Logger, flock func(fd int, how int) error,
) (*dataDirLock, error) {
	path, err := syncStoreLockPath(dataDir, dbPath)
	if err != nil {
		return nil, err
	}

	return lockDataDirFile(path, lockTarget{inUse: ErrSyncStoreInUse, noun: "sync state", name: dbPath}, logger, flock)
}

// lockDataDirFile takes the lock file at path, which guards target. A lock
// the filesystem cannot provide is warned about and the caller continues
// unlocked, because refusing to sync such folders at all would be worse
// than the risk it guards.
func lockDataDirFile(
	path string, target lockTarget, logger *slog.Logger, flock func(fd int, how int) error,
) (*dataDirLock, error) {
	if flock == nil {
		flock = syscall.Flock
	}
	if mkErr := localpath.MkdirAll(filepath.Dir(path), engineLocksDirPerm); mkErr != nil {
		return nil, fmt.Errorf("create lock directory: %w", mkErr)
	}
	file, err := localpath.OpenFile(path, os.O_RDWR|os.O_CREATE, engineLockPerm)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", target.noun, err)
	}

	lockErr := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case lockErr == nil:
		return &dataDirLock{file: file, flock: flock}, nil
	case errors.Is(lockErr, syscall.EWOULDBLOCK):
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("%w: %s", target.inUse, target.name), closeErr)
	case isUnsupportedLockErrno(lockErr):
		if logger != nil {
			logger.Warn("onedrive-go cannot lock its data folder, so it cannot stop a second sync from using this "+
				target.noun+" at the same time; make sure only one sync runs here",
				slog.String("path", target.name),
				slog.String("lock", path),
				slog.Any("error", lockErr),
			)
		}
		return &dataDirLock{file: file}, nil
	default:
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("lock %s %s: %w", target.noun, target.name, lockErr), closeErr)
	}
}

// LockDriveForOfflineChange takes, outside any engine and in the engine's
// order, the locks an engine using the state store at dbPath holds, for an
// offline change to that store: drive reset-sync-state, drive rebind-root,
// the purge of a drive's state, a shortcut child's cleanup, a rename for an
// email change. dataDir must be the data directory the engines use.
//
// It returns ErrSyncStoreInUse while an engine in any process has the store
// open: one still draining or closing after its owner stopped answering on
// the control socket, one run by another configuration sharing dataDir with
// another sync folder for the drive, and one started before its folder was
// moved or replaced (the case rebind-root is for), none of which the folder
// lock sees. It returns ErrSyncRootInUse while an engine holds the folder
// now at syncRoot. Holding both keeps a new engine from starting on the
// store or the folder during the change.
//
// An empty syncRoot, or a missing folder there, adds no folder lock: no
// engine can start on it. The store lock needs no store: an engine starting
// meanwhile would create one. On a filesystem that cannot lock, each lock is
// warned about and the change goes ahead, as an engine does. No attempt
// waits. The returned release drops the store lock, then the folder lock.
func LockDriveForOfflineChange(
	ctx context.Context,
	dataDir, syncRoot, dbPath string,
	logger *slog.Logger,
) (release func() error, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("lock sync state: %w", ctxErr)
	}

	var locks engineLocks
	if syncRoot != "" {
		root, rootErr := acquireSyncRootLock(dataDir, syncRoot, logger, nil)
		switch {
		case rootErr == nil:
			locks.root = root
		case errors.Is(rootErr, os.ErrNotExist):
			// Nothing at the path: no engine of this configuration can start.
		default:
			return nil, rootErr
		}
	}

	store, err := acquireSyncStoreLock(dataDir, dbPath, logger, nil)
	if err != nil {
		return nil, errors.Join(err, locks.release())
	}
	locks.store = store

	return locks.release, nil
}

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
// can be (foldStateDBLog), so the store is one file and moves in one rename;
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

	pending, leftovers, err := splitStateDBMoves(moves)
	if err != nil {
		return err
	}
	if checkErr := checkStateDBMoveTargets(pending, begin == nil); checkErr != nil {
		return checkErr
	}
	if begin != nil {
		for _, move := range pending {
			if removeErr := removeStateDBSidecars(move.To); removeErr != nil {
				return removeErr
			}
		}
		if beginErr := begin(); beginErr != nil {
			return fmt.Errorf("record the account email change: %w", beginErr)
		}
	}

	for _, move := range pending {
		foldStateDBLogBeforeMove(ctx, move, logger)
		if moveErr := moveStateDBFamily(move.From, move.To); moveErr != nil {
			return moveErr
		}
		logger.Info("moved sync state for an account email change",
			slog.String("from", move.From), slog.String("to", move.To))
	}

	for _, dbPath := range leftovers {
		if removeErr := removeStateDBLeftovers(dbPath, logger); removeErr != nil {
			return removeErr
		}
	}

	return nil
}

// splitStateDBMoves separates the moves whose source has a database from the
// sources that hold only leftovers of a deleted store, or nothing.
func splitStateDBMoves(moves []config.StateDBMove) (pending []config.StateDBMove, leftovers []string, err error) {
	pending = make([]config.StateDBMove, 0, len(moves))
	for _, move := range moves {
		exists, statErr := stateDBFileExists(move.From)
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
		exists, err := stateDBFileExists(move.To)
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
		if exists, err := stateDBFileExists(sidecar); err == nil && exists {
			hasSidecar = true
			break
		}
	}
	if !hasSidecar {
		return
	}
	if targetFound, err := stateDBFamilyExists(move.To); err != nil || targetFound {
		return
	}

	if err := foldStateDBLog(ctx, move.From); err != nil {
		logger.Warn("could not fold the sync state log into its database before an account email change "+
			"moves it; moving its files as they are",
			slog.String("db_path", move.From), slog.Any("error", err))
	}
}

// refuseStoreOfUnfinishedEmailChange returns ErrEmailChangeUnfinished when
// an unfinished account email change in dataDir moves the store at dbPath,
// from or to that name, compared by store lock identity. The caller holds
// the store's lock: the mover records a change while it holds the locks of
// every store the change moves, so either the mover saw this engine's lock
// and refused, or this check sees the journal. A journal that cannot be read
// may name any store and refuses too.
func refuseStoreOfUnfinishedEmailChange(dataDir, dbPath string) error {
	moves, err := config.UnfinishedEmailChangeStateDBs(dataDir)
	if err != nil {
		return fmt.Errorf("check for an unfinished account email change: %w", err)
	}
	if len(moves) == 0 {
		return nil
	}

	own, err := syncStoreLockPath(dataDir, dbPath)
	if err != nil {
		return err
	}
	for _, move := range moves {
		for _, path := range []string{move.From, move.To} {
			named, lockErr := syncStoreLockPath(dataDir, path)
			if lockErr != nil {
				return lockErr
			}
			if named == own {
				return fmt.Errorf("%w: it moves %s", ErrEmailChangeUnfinished, dbPath)
			}
		}
	}

	return nil
}

// removeStateDBLeftovers deletes the sidecars at dbPath, a name that has no
// database. The caller holds the store lock.
func removeStateDBLeftovers(dbPath string, logger *slog.Logger) error {
	found, err := stateDBFamilyExists(dbPath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := removeStateDBFiles(dbPath); err != nil {
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

// foreignSyncClientMarkers are files other sync clients keep at the top of
// the folders they manage. Two clients on one folder each see the other's
// changes as user edits and can delete or duplicate content.
func foreignSyncClientMarkers() []string {
	return []string{".dropbox", ".dropbox.cache", ".stfolder", ".nextcloudsync.log", ".owncloudsync.log"}
}

// macOSCloudStorageDir is where macOS File Provider clients (Dropbox, Google
// Drive, Microsoft's own OneDrive) keep their folders.
const macOSCloudStorageDir = "Library/CloudStorage"

// warnAboutForeignSyncClients warns once per engine start when the sync root
// looks managed by another sync client.
func warnAboutForeignSyncClients(root string, logger *slog.Logger) {
	if logger == nil {
		return
	}

	var found []string
	for _, marker := range foreignSyncClientMarkers() {
		if _, err := localpath.Lstat(filepath.Join(root, marker)); err == nil {
			found = append(found, marker)
		}
	}
	if strings.Contains(filepath.ToSlash(root), "/"+macOSCloudStorageDir+"/") {
		found = append(found, "~/"+macOSCloudStorageDir)
	}
	if len(found) == 0 {
		return
	}

	logger.Warn("This sync folder also looks managed by another sync client ("+strings.Join(found, ", ")+
		"); two clients syncing one folder can undo or duplicate each other's changes",
		slog.String("path", root),
	)
}

// releaseEngineLocksOnStartupFailure releases the engine's locks when engine
// construction fails after taking them; ownedByEngine flips to true once
// the engine owns the locks (and the store) and releases them in Close. It
// is deferred before the store opens, so it runs after the store's own
// startup-failure close.
func releaseEngineLocksOnStartupFailure(logger *slog.Logger, locks engineLocks, ownedByEngine *bool) {
	if *ownedByEngine {
		return
	}
	if err := locks.release(); err != nil && logger != nil {
		logger.Debug("release engine locks after startup failure", slog.Any("error", err))
	}
}

// release drops the lock: an explicit unlock (when the lock was taken),
// then the close. Safe on nil and after an earlier release.
func (l *dataDirLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	var err error
	if l.flock != nil {
		if unlockErr := l.flock(int(l.file.Fd()), syscall.LOCK_UN); unlockErr != nil {
			err = fmt.Errorf("unlock: %w", unlockErr)
		}
	}
	if closeErr := l.file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close lock file: %w", closeErr))
	}
	l.file = nil

	return err
}
