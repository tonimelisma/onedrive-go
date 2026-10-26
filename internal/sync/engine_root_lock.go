package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ErrSyncRootInUse means another onedrive-go engine, in this process or
// another, already holds the sync root. The control-socket owner lock only
// covers one control socket, so two configurations sharing a data directory
// could each start an engine on one folder and fight over it (D-083).
var ErrSyncRootInUse = errors.New("another onedrive-go sync is already syncing this folder")

// syncRootLocksDir is the data-directory subfolder that holds root locks.
const syncRootLocksDir = "locks"

// syncRootLockPerm and syncRootLocksDirPerm keep lock files private.
const (
	syncRootLockPerm     = 0o600
	syncRootLocksDirPerm = 0o700
)

// syncRootLock is an exclusive flock on a regular file in the data
// directory named after the sync root's (st_dev, st_ino), held for the
// engine's lifetime. Keying by inode means every path, symlink or bind mount
// that reaches the folder contends for the same lock. The lock file lives in
// the data directory, not the sync folder, because flock on the folder
// itself fails on Linux NFS (EBADF on a read-only directory descriptor) and
// on some FUSE filesystems (ENOSYS), which made the engine refuse folders it
// used to sync. Lock files are never removed: unlinking one while another
// process may hold a descriptor to it would split the lock in two.
type syncRootLock struct {
	file  *os.File
	flock func(fd int, how int) error
}

// syncRootLockDataDir is where root locks live: the engine's data dir, or
// the state DB's folder (which is inside it) when no data dir is set.
func syncRootLockDataDir(cfg *engineInputs) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}

	return filepath.Dir(cfg.DBPath)
}

// syncRootLockPath names the lock file for root inside dataDir.
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
	return filepath.Join(dataDir, syncRootLocksDir, name), nil
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

// acquireSyncRootLock takes the root lock. It must run before the state
// store opens: a second engine is refused before it touches the store. A
// lock the filesystem cannot provide is warned about and the engine
// continues unlocked, because refusing to sync such folders at all would be
// worse than the risk it guards. flock is the syscall (a test seam).
func acquireSyncRootLock(
	dataDir, root string, logger *slog.Logger, flock func(fd int, how int) error,
) (*syncRootLock, error) {
	if flock == nil {
		flock = syscall.Flock
	}

	path, err := syncRootLockPath(dataDir, root)
	if err != nil {
		return nil, err
	}
	if mkErr := localpath.MkdirAll(filepath.Dir(path), syncRootLocksDirPerm); mkErr != nil {
		return nil, fmt.Errorf("create sync folder lock directory: %w", mkErr)
	}
	file, err := localpath.OpenFile(path, os.O_RDWR|os.O_CREATE, syncRootLockPerm)
	if err != nil {
		return nil, fmt.Errorf("open sync folder lock: %w", err)
	}

	lockErr := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case lockErr == nil:
		return &syncRootLock{file: file, flock: flock}, nil
	case errors.Is(lockErr, syscall.EWOULDBLOCK):
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("%w: %s", ErrSyncRootInUse, root), closeErr)
	case isUnsupportedLockErrno(lockErr):
		if logger != nil {
			logger.Warn("onedrive-go cannot lock its data folder, so it cannot stop a second sync from using "+
				"this folder at the same time; make sure only one sync runs here",
				slog.String("path", root),
				slog.String("lock", path),
				slog.String("error", lockErr.Error()),
			)
		}
		return &syncRootLock{file: file}, nil
	default:
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("lock sync folder %s: %w", root, lockErr), closeErr)
	}
}

// releaseSyncRootLockOnStartupFailure releases the root lock when engine
// construction fails after taking it; ownedByEngine flips to true once the
// engine owns the lock (and the store) and releases it in Close. It is
// deferred before the store opens, so it runs after the store's own
// startup-failure close.
func releaseSyncRootLockOnStartupFailure(logger *slog.Logger, lock *syncRootLock, ownedByEngine *bool) {
	if *ownedByEngine {
		return
	}
	if err := lock.release(); err != nil && logger != nil {
		logger.Debug("release sync folder lock after startup failure", slog.String("error", err.Error()))
	}
}

// release drops the lock: an explicit unlock (when the lock was taken),
// then the close.
func (l *syncRootLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	var err error
	if l.flock != nil {
		if unlockErr := l.flock(int(l.file.Fd()), syscall.LOCK_UN); unlockErr != nil {
			err = fmt.Errorf("unlock sync folder: %w", unlockErr)
		}
	}
	if closeErr := l.file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close sync folder lock: %w", closeErr))
	}
	l.file = nil

	return err
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
