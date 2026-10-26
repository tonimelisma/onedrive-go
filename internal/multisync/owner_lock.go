package multisync

import (
	"errors"
	"fmt"
	"os"
	gosync "sync"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// controlLockSuffix names the owner lock beside the control socket
// (control.sock -> control.sock.lock).
const controlLockSuffix = ".lock"

// maxOwnerLockAttempts bounds the retry when the lock file is replaced
// between our open and our flock (see acquireOwnerLock). Each retry needs a
// competing owner to unlink and recreate the file in that window, so a
// handful is generous.
const maxOwnerLockAttempts = 8

// ownerLock is the durable proof that this process owns the control socket.
// Before it existed, ownership was "the socket file answers a dial": two
// syncs starting together both failed to dial a stale socket, both removed
// it, and one deleted the other's live socket, leaving two owners syncing
// the same drives (D-083). flock is released by the kernel when the process
// dies, so a crashed owner never blocks the next one, and taking it before
// any stale-socket removal makes that removal safe.
type ownerLock struct {
	path string
	// mu guards file: Close runs from both the context-cancel goroutine and
	// the owner's explicit shutdown.
	mu   gosync.Mutex
	file *os.File
}

// acquireOwnerLock takes an exclusive, non-blocking flock on path. It
// returns ControlSocketInUseError when another live process holds it.
//
// The lock file is removed on release, so a contender can open the old
// inode just before it is unlinked and then lock that orphan. After locking,
// the descriptor's inode is compared with the path's: a mismatch means the
// lock is on a file nobody else will ever see, so it is dropped and retried.
func acquireOwnerLock(path, socketPath string) (*ownerLock, error) {
	for range maxOwnerLockAttempts {
		file, err := localpath.OpenFile(path, os.O_RDWR|os.O_CREATE, controlSocketFilePerm)
		if err != nil {
			return nil, fmt.Errorf("open control lock %s: %w", path, err)
		}

		if lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
			closeErr := file.Close()
			if errors.Is(lockErr, syscall.EWOULDBLOCK) {
				return nil, &ControlSocketInUseError{Path: socketPath}
			}
			return nil, errors.Join(fmt.Errorf("lock control lock %s: %w", path, lockErr), closeErr)
		}

		same, sameErr := lockFileStillAtPath(file, path)
		if sameErr != nil {
			return nil, errors.Join(sameErr, file.Close())
		}
		if same {
			return &ownerLock{path: path, file: file}, nil
		}

		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("close superseded control lock %s: %w", path, closeErr)
		}
	}

	return nil, fmt.Errorf("lock control lock %s: it kept being replaced by another process", path)
}

func lockFileStillAtPath(file *os.File, path string) (bool, error) {
	held, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat held control lock: %w", err)
	}

	current, err := localpath.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat control lock %s: %w", path, err)
	}

	return os.SameFile(held, current), nil
}

// release removes the lock file while still holding the lock, then drops the
// lock. Removing first means the next owner creates a fresh file instead of
// inheriting this one, and the inode check in acquireOwnerLock rejects any
// contender that opened this file before the unlink.
func (l *ownerLock) release() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}

	var err error
	if removeErr := localpath.Remove(l.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = fmt.Errorf("remove control lock: %w", removeErr)
	}
	if closeErr := l.file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close control lock: %w", closeErr))
	}
	l.file = nil

	return err
}
