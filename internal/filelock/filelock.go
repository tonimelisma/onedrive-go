// Package filelock is the one place the repository takes advisory file locks:
// an exclusive flock(2) on an open file, tried without waiting, released,
// waited for under a context, and the classification of a filesystem that
// cannot lock at all. It is a leaf: it imports no internal package. Each
// caller owns its own lock file (where it lives, when it is created and
// removed) and its own policy for a lock it cannot get.
//
// flock locks belong to an open file description: two descriptors opened
// separately on one file exclude each other even inside one process, and the
// kernel drops a lock when its last descriptor closes, so a crashed holder
// never blocks the next one.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Flock is the flock(2) call. Callers keep it as a test seam; nil means
// syscall.Flock.
type Flock func(fd int, how int) error

func (f Flock) call(fd, how int) error {
	if f == nil {
		return syscall.Flock(fd, how) //nolint:wrapcheck // the errno is the answer callers classify
	}

	return f(fd, how)
}

// TryLock takes an exclusive lock on fd without waiting. It reports false
// with no error when another open file description holds the lock. Any other
// failure is returned as flock gave it; IsUnsupported tells the ones that
// mean the filesystem cannot lock.
func TryLock(flock Flock, fd int) (bool, error) {
	err := flock.call(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

// Unlock releases the lock on fd. Closing the last descriptor releases it as
// well; callers that keep the file open after the critical section unlock
// explicitly.
func Unlock(flock Flock, fd int) error {
	return flock.call(fd, syscall.LOCK_UN)
}

// Wait takes the lock on fd, trying again every poll while another process
// holds it, until it is granted or ctx ends. A blocking flock could not be
// bounded by a context, so the wait polls. A wait ctx ended returns an error
// wrapping context.Cause(ctx); any other flock failure ends the wait at once.
func Wait(ctx context.Context, flock Flock, fd int, poll time.Duration) error {
	for {
		locked, err := TryLock(flock, fd)
		if err != nil || locked {
			return err
		}

		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for the lock: %w", context.Cause(ctx))
		case <-timer.C:
		}
	}
}

// IsUnsupported reports a flock failure that means "this filesystem cannot
// lock" (some network and FUSE filesystems), as opposed to a held lock or an
// I/O error.
func IsUnsupported(err error) bool {
	for _, errno := range []syscall.Errno{
		syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK, syscall.EINVAL, syscall.EBADF, syscall.ENOSYS,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	return false
}
