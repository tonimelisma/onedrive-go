package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidFilePermissions matches the standard config file permissions (owner rw, group/other r).
const pidFilePermissions = 0o644

// pidDirPermissions matches the standard directory permissions (owner rwx, group/other rx).
const pidDirPermissions = 0o755

// writePIDFile writes the current process ID to path and acquires an exclusive
// flock. Returns a cleanup function that removes the file and releases the
// lock. If the lock cannot be acquired, another daemon is already running.
func writePIDFile(path string) (cleanup func(), err error) {
	if path == "" {
		return nil, fmt.Errorf("PID file path is empty — cannot determine data directory")
	}

	dir := filepath.Dir(path)
	if mkdirErr := os.MkdirAll(dir, pidDirPermissions); mkdirErr != nil {
		return nil, fmt.Errorf("creating PID file directory: %w", mkdirErr)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, pidFilePermissions)
	if err != nil {
		return nil, fmt.Errorf("opening PID file: %w", err)
	}

	// Non-blocking exclusive lock — fails immediately if another process holds it.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()

		return nil, fmt.Errorf("another sync --watch is already running (could not lock %s)", path)
	}

	// Truncate and write current PID.
	if err := f.Truncate(0); err != nil {
		f.Close()

		return nil, fmt.Errorf("truncating PID file: %w", err)
	}

	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()

		return nil, fmt.Errorf("writing PID file: %w", err)
	}

	// Sync to disk so readers see the PID immediately.
	if err := f.Sync(); err != nil {
		f.Close()

		return nil, fmt.Errorf("syncing PID file: %w", err)
	}

	return func() {
		os.Remove(path)
		f.Close()
	}, nil
}

// readPIDFile reads the PID from the given file path. Returns 0 and an error
// if the file does not exist or contains invalid content.
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID in %s: %w", path, err)
	}

	return pid, nil
}

// sendSIGHUP reads the PID from the daemon PID file and sends SIGHUP to the
// running daemon. If the PID file does not exist or the process is not alive,
// returns a descriptive error. Stale PID files (process dead) are cleaned up.
func sendSIGHUP(pidPath string) error {
	pid, err := readPIDFile(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no running daemon found (no PID file at %s)", pidPath)
		}

		return err
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// Check if the process is alive with signal 0.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is dead — clean up stale PID file.
		os.Remove(pidPath)

		return fmt.Errorf("daemon (PID %d) is not running (stale PID file removed)", pid)
	}

	if err := proc.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("sending SIGHUP to daemon (PID %d): %w", pid, err)
	}

	return nil
}
