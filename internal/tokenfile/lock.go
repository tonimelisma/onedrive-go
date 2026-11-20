package tokenfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/filelock"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// LockSuffix names the lock file of a sign-in file, beside it:
// token_personal_x.json -> token_personal_x.json.lock. Token discovery takes
// only names ending in ".json", so it never reads a lock file as a sign-in.
const LockSuffix = ".lock"

// lockPoll is the pause between attempts while another process holds the
// lock: a refresh holds it for one sign-in request, so a few tries a second
// is plenty.
const lockPoll = 50 * time.Millisecond

// ErrLockUnsupported means the filesystem holding the sign-in file cannot
// lock. The caller goes on without the lock; re-reading the file after a
// refused refresh token is the protection left.
var ErrLockUnsupported = errors.New("the filesystem cannot lock the sign-in file")

// ErrLockWaitEnded means another process held the sign-in lock for as long
// as the caller's context allowed: another process is refreshing the sign-in.
var ErrLockWaitEnded = errors.New("another onedrive-go process is refreshing the sign-in")

// AcquireLock takes the lock of the sign-in file at tokenPath, the one
// authority between processes over that file: every read-refresh-write of a
// refresh, a login's install and a logout's delete hold it. It waits while
// another process holds it, until ctx ends (ErrLockWaitEnded); the caller
// bounds the wait. The lock file and its folder are created as needed.
//
// release drops the lock; it is never nil. A filesystem that cannot lock
// returns a no-op release with an error wrapping ErrLockUnsupported.
func AcquireLock(ctx context.Context, tokenPath string) (release func() error, err error) {
	noop := func() error { return nil }

	root, name, err := fsroot.OpenPath(tokenPath + LockSuffix)
	if err != nil {
		return noop, fmt.Errorf("tokenfile: opening lock root: %w", err)
	}
	if mkErr := root.MkdirAll(DirPerms); mkErr != nil {
		return noop, fmt.Errorf("tokenfile: creating lock folder: %w", mkErr)
	}
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, FilePerms)
	if err != nil {
		return noop, fmt.Errorf("tokenfile: opening lock %s%s: %w", tokenPath, LockSuffix, err)
	}

	fd := int(file.Fd())
	if waitErr := filelock.Wait(ctx, nil, fd, lockPoll); waitErr != nil {
		closeErr := file.Close()
		switch {
		case filelock.IsUnsupported(waitErr):
			return noop, errors.Join(fmt.Errorf("%w: %s: %w", ErrLockUnsupported, tokenPath, waitErr), closeErr)
		case ctx.Err() != nil:
			return noop, errors.Join(fmt.Errorf("%w: %s: %w", ErrLockWaitEnded, tokenPath, waitErr), closeErr)
		default:
			return noop, errors.Join(fmt.Errorf("tokenfile: locking %s: %w", tokenPath, waitErr), closeErr)
		}
	}

	return func() error {
		unlockErr := filelock.Unlock(nil, fd)
		closeErr := file.Close()
		if unlockErr != nil {
			unlockErr = fmt.Errorf("tokenfile: unlocking %s: %w", tokenPath, unlockErr)
		}

		return errors.Join(unlockErr, closeErr)
	}, nil
}

// RemoveLock deletes the lock file of tokenPath, for a purge that removes
// everything an account left. A missing one is not an error.
func RemoveLock(tokenPath string) error {
	root, name, err := fsroot.OpenPath(tokenPath + LockSuffix)
	if err != nil {
		return fmt.Errorf("tokenfile: opening lock root: %w", err)
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("tokenfile: removing lock of %s: %w", tokenPath, err)
	}

	return nil
}

// installLockWait bounds a login's or a logout's wait for a refresh in
// another process to finish with the sign-in file. It matches the refresh's
// own bound (graph's signInLockWait), which outlasts one sign-in request.
const installLockWait = 90 * time.Second

// WithLock runs fn holding the lock of the sign-in file at tokenPath, for a
// change of the file that is not a refresh: a login installing its sign-in,
// or undoing that. It waits for the lock no longer than ctx and
// installLockWait allow; on a filesystem that cannot lock, fn runs unlocked.
func WithLock(ctx context.Context, tokenPath string, fn func() error) (err error) {
	lockCtx, cancel := context.WithTimeout(ctx, installLockWait)
	defer cancel()

	release, err := AcquireLock(lockCtx, tokenPath)
	if err != nil && !errors.Is(err, ErrLockUnsupported) {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	return fn()
}

// Install moves the sign-in at pendingPath to tokenPath under tokenPath's
// lock: an atomic write of the final file (directory sync included), then
// the removal of the pending file. A process refreshing the same account
// meanwhile reads either the old sign-in or the new one, and its refresh
// waits for the install instead of racing it. ctx bounds the wait for the
// lock.
//
// undo puts back what tokenPath held, read under the same lock as the
// install, so it is the sign-in the install replaced and not one a refresh
// had already replaced; it removes the file when there was none. It takes the
// lock again, waiting up to installLockWait even when ctx has ended: a login
// that was canceled still puts back the sign-in it replaced.
func Install(ctx context.Context, pendingPath, tokenPath string) (undo func() error, err error) {
	tok, err := Load(pendingPath)
	if err != nil {
		return nil, err
	}

	root, name, err := fsroot.OpenPath(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("tokenfile: opening token root: %w", err)
	}

	var previous []byte
	hadPrevious := false
	err = WithLock(ctx, tokenPath, func() error {
		data, readErr := root.ReadFile(name)
		switch {
		case readErr == nil:
			previous, hadPrevious = data, true
		case !errors.Is(readErr, fs.ErrNotExist):
			return fmt.Errorf("tokenfile: reading the previous sign-in %s: %w", tokenPath, readErr)
		}
		if saveErr := Save(tokenPath, tok); saveErr != nil {
			return saveErr
		}
		if delErr := Delete(pendingPath); delErr != nil && !errors.Is(delErr, ErrNotFound) {
			return delErr
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return func() error {
		return WithLock(context.WithoutCancel(ctx), tokenPath, func() error {
			if !hadPrevious {
				if delErr := Delete(tokenPath); delErr != nil && !errors.Is(delErr, ErrNotFound) {
					return delErr
				}

				return nil
			}
			if writeErr := root.AtomicWrite(name, previous, FilePerms, DirPerms, ".token-*.tmp"); writeErr != nil {
				return fmt.Errorf("tokenfile: restoring %s: %w", tokenPath, writeErr)
			}

			return nil
		})
	}, nil
}
