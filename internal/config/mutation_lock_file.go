package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// configLockFileName is the lock file a config or catalog change takes in
// the directory it changes when that directory's filesystem cannot flock.
const configLockFileName = ".onedrive-go.lock"

// configLockFilePermissions keeps the lock file, which names a host and a
// process, private to the user.
const configLockFilePermissions = 0o600

// lockFileOwner is the record a lock file holds: who took it and when.
type lockFileOwner struct {
	PID   int    `json:"pid"`
	Host  string `json:"host"`
	Since string `json:"since"`
}

// lockFileIdentity names this process for a lock file it takes, and judges
// whether the process a lock file names still runs. The fields are the
// seams of the tests; production uses defaultLockFileIdentity.
type lockFileIdentity struct {
	pid      int
	hostname func() (string, error)
	// running reports whether a process of this host with pid still runs.
	running func(pid int) bool
	now     func() time.Time
}

func defaultLockFileIdentity() lockFileIdentity {
	return lockFileIdentity{
		pid:      os.Getpid(),
		hostname: os.Hostname,
		running:  processRunning,
		now:      time.Now,
	}
}

// processRunning reports whether pid names a running process of this host:
// signal 0 checks without sending anything, and a process another user owns
// answers EPERM, which still means it runs. A pid since reused by another
// process reads as running, the direction that waits rather than takes.
func processRunning(pid int) bool {
	err := syscall.Kill(pid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// withLockFile runs fn while holding the lock file of dir, the lock of a
// directory whose filesystem cannot flock. The file is created with
// O_CREATE|O_EXCL, which NFS (version 3 and later) and FUSE filesystems
// carry out atomically: exactly one process creates it. It records the
// holder, and is removed when fn returns.
//
// A lock file is never taken over, not even one whose holder has stopped:
// two processes that both judged it stale could each remove the lock the
// other had just taken, and both would change the file. The bounded wait
// ends instead with ErrConfigLocked, naming the holder and, when that
// process no longer runs on this host, the file to remove.
func (l configLocker) withLockFile(ctx context.Context, dir string, fn func() error) (err error) {
	path := filepath.Join(dir, configLockFileName)
	if takeErr := l.takeLockFile(ctx, path); takeErr != nil {
		return takeErr
	}
	defer func() {
		if removeErr := localpath.Remove(path); removeErr != nil && err == nil {
			err = fmt.Errorf("releasing lock file %s: %w", path, removeErr)
		}
	}()

	return fn()
}

// takeLockFile creates path for this process, polling like acquire until it
// can or the bounded wait is over.
func (l configLocker) takeLockFile(ctx context.Context, path string) error {
	record, err := l.lockFileRecord()
	if err != nil {
		return err
	}
	maxWaits := int(l.timeout / l.pollInterval)
	for waits := 0; ; waits++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("lock %s: %w", path, ctxErr)
		}
		created, createErr := createLockFile(path, record)
		switch {
		case createErr != nil:
			return createErr
		case created:
			return nil
		case waits >= maxWaits:
			return l.lockFileHeldError(path)
		}

		if sleepErr := l.sleep(ctx, l.pollInterval); sleepErr != nil {
			return fmt.Errorf("lock %s: %w", path, sleepErr)
		}
	}
}

func (l configLocker) lockFileRecord() ([]byte, error) {
	host, err := l.lockFile.hostname()
	if err != nil {
		return nil, fmt.Errorf("naming this host for a lock file: %w", err)
	}
	record, err := json.Marshal(lockFileOwner{
		PID: l.lockFile.pid, Host: host, Since: l.lockFile.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding lock file record: %w", err)
	}

	return record, nil
}

// createLockFile creates path holding record, and reports false, with no
// error, when the file exists already. A file it created but could not write
// whole is removed again, so no half record is ever left as a lock.
func createLockFile(path string, record []byte) (bool, error) {
	file, err := localpath.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, configLockFilePermissions)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating lock file %s: %w", path, err)
	}
	_, writeErr := file.Write(record)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return false, errors.Join(fmt.Errorf("writing lock file %s: %w", path, writeErr), localpath.Remove(path))
	}

	return true, nil
}

// lockFileHeldError is the end of a wait for a lock file: ErrConfigLocked,
// naming the holder the file records, and the file to remove when that
// holder is a process of this host that no longer runs.
func (l configLocker) lockFileHeldError(path string) error {
	data, readErr := localpath.ReadFile(path)
	var owner lockFileOwner
	if readErr != nil || json.Unmarshal(data, &owner) != nil || owner.PID <= 0 || owner.Host == "" {
		return fmt.Errorf("%s is %w: its lock file %s stayed in place for %s and names no holder; "+
			"if no onedrive-go command is running, remove it and try again", filepath.Dir(path), ErrConfigLocked, path, l.timeout)
	}
	holder := "process " + strconv.Itoa(owner.PID) + " on " + owner.Host + " since " + owner.Since
	host, hostErr := l.lockFile.hostname()
	if hostErr == nil && host == owner.Host && !l.lockFile.running(owner.PID) {
		return fmt.Errorf("%s is %w by %s, which no longer runs; remove %s and try again",
			filepath.Dir(path), ErrConfigLocked, holder, path)
	}

	return fmt.Errorf("%s is %w by %s: it stayed locked for %s; let that onedrive-go command finish, then try again "+
		"(if it stopped without finishing, remove %s)", filepath.Dir(path), ErrConfigLocked, holder, l.timeout, path)
}
