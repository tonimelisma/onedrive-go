package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	// configLockTimeout bounds how long a config or catalog change waits for
	// another onedrive-go process to finish its own change. Every holder only
	// rewrites one small file, so a lock held this long belongs to a stopped
	// or stuck process, not a slow one.
	configLockTimeout = 10 * time.Second
	// configLockPollInterval is the pause between non-blocking lock attempts.
	configLockPollInterval = 50 * time.Millisecond
)

// ErrConfigLocked means another process held the config or catalog lock for
// the whole bounded wait, so the change was not made. The error that carries
// it names the locked directory. The CLI reports it as an ordinary failure
// (exit 1, it may work if run again); the daemon logs it and carries on.
var ErrConfigLocked = errors.New("locked by another onedrive-go process")

// configLocker takes the advisory lock that serializes read-modify-write
// changes of the managed files in one directory: config.toml in the config
// directory, catalog.json in the data directory (on macOS both are the same
// directory, and so the same lock). The fields are the seams of the bounded
// wait; production uses defaultConfigLocker.
//
// flock locks belong to an open file description, and every acquisition here
// opens the directory afresh, so a function that holds a lock must never take
// the same directory's lock again: the inner call would wait on the outer one
// and fail after the timeout. Locked callbacks only read and rewrite their
// own file, with one exception. An account email change holds the lock on its
// own folder (emailChanger.withLock), a directory no other lock names, around
// steps that take the config lock or the catalog lock one after the other.
// Nothing that holds the config lock or the catalog lock takes the email
// change lock.
type configLocker struct {
	flock        func(fd int, how int) error
	sleep        func(time.Duration)
	timeout      time.Duration
	pollInterval time.Duration
}

func defaultConfigLocker() configLocker {
	return configLocker{
		flock:        syscall.Flock,
		sleep:        time.Sleep,
		timeout:      configLockTimeout,
		pollInterval: configLockPollInterval,
	}
}

// withConfigMutationLock serializes config read-modify-write entrypoints that
// participate in this package, with the default bounded wait.
func withConfigMutationLock(path string, createDir bool, fn func() error) error {
	return defaultConfigLocker().withMutationLock(path, createDir, fn)
}

// withMutationLock runs fn while holding the lock on path's parent directory.
// Existing-file operations lock the parent directory when it exists, even if
// the file itself is currently missing. If the parent directory is missing and
// createDir is false, the mutation produces its historical missing-file result
// instead of creating directories just to lock. fn never runs when the lock
// was not taken.
func (l configLocker) withMutationLock(path string, createDir bool, fn func() error) (err error) {
	root, _, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening lock root: %w", err)
	}
	if createDir {
		if mkErr := root.MkdirAll(configDirPermissions); mkErr != nil {
			return fmt.Errorf("creating lock directory: %w", mkErr)
		}
	}

	dir := filepath.Dir(path)
	lockFile, err := localpath.Open(dir)
	if err != nil {
		if !createDir && errors.Is(err, os.ErrNotExist) {
			return fn()
		}
		return fmt.Errorf("opening lock directory: %w", err)
	}
	defer func() {
		closeErr := lockFile.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf("closing lock directory %s: %w", dir, closeErr)
		}
	}()

	fd := int(lockFile.Fd())
	if lockErr := l.acquire(fd, dir); lockErr != nil {
		return lockErr
	}
	defer func() {
		unlockErr := l.flock(fd, syscall.LOCK_UN)
		if err == nil && unlockErr != nil {
			err = fmt.Errorf("unlocking %s: %w", dir, unlockErr)
		}
	}()

	return fn()
}

// acquire polls a non-blocking exclusive flock on fd every pollInterval until
// it is granted or timeout has been waited out. A blocking flock would hang
// every config change, and the daemon's reload, behind a stopped or stuck
// holder for as long as that holder lives.
func (l configLocker) acquire(fd int, dir string) error {
	maxWaits := int(l.timeout / l.pollInterval)
	for waits := 0; ; waits++ {
		err := l.flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return fmt.Errorf("locking %s: %w", dir, err)
		case waits >= maxWaits:
			return fmt.Errorf("%s is %w: it stayed locked for %s; let the other onedrive-go command finish, then try again",
				dir, ErrConfigLocked, l.timeout)
		}

		l.sleep(l.pollInterval)
	}
}

func (l configLocker) setDriveKey(path string, canonicalID driveid.CanonicalID, key, value string) error {
	return l.withMutationLock(path, false, func() error {
		return setDriveKeySnapshot(path, canonicalID, key, value)
	})
}

// updateCatalog is the catalog's read-modify-write: load, update, save, all
// under the lock on the catalog's directory, so two processes (or two
// goroutines) updating the catalog together cannot lose either update. update
// must not take a config or catalog lock itself (see configLocker).
func (l configLocker) updateCatalog(dataDir string, update func(*Catalog) error) error {
	path := CatalogPathForDataDir(dataDir)
	if path == "" {
		// No data directory: there is no file to protect, and the save below
		// writes nothing.
		return loadUpdateSaveCatalog(dataDir, update)
	}

	return l.withMutationLock(path, true, func() error {
		return loadUpdateSaveCatalog(dataDir, update)
	})
}

func loadUpdateSaveCatalog(dataDir string, update func(*Catalog) error) error {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return err
	}
	if err := update(catalog); err != nil {
		return err
	}

	return SaveCatalogForDataDir(dataDir, catalog)
}
