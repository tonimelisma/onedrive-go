package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWritePIDFile_CreatesFileWithCurrentPID(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")

	cleanup, err := writePIDFile(path)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	defer cleanup()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), pid)
}

func TestWritePIDFile_FlockPreventsSecondAcquisition(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")

	cleanup1, err := writePIDFile(path)
	require.NoError(t, err)
	require.NotNil(t, cleanup1)

	defer cleanup1()

	// Second attempt should fail because the flock is held.
	cleanup2, err := writePIDFile(path)
	require.Error(t, err)
	assert.Nil(t, cleanup2)
	assert.Contains(t, err.Error(), "already running")
}

func TestWritePIDFile_CleanupRemovesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")

	cleanup, err := writePIDFile(path)
	require.NoError(t, err)

	cleanup()

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestWritePIDFile_EmptyPathReturnsError(t *testing.T) {
	t.Parallel()

	cleanup, err := writePIDFile("")
	assert.Error(t, err)
	assert.Nil(t, cleanup)
	assert.Contains(t, err.Error(), "empty")
}

func TestWritePIDFile_CreatesParentDirectories(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "dir", "daemon.pid")

	cleanup, err := writePIDFile(path)
	require.NoError(t, err)

	defer cleanup()

	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestReadPIDFile_ReadsValidPID(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")
	require.NoError(t, os.WriteFile(path, []byte("12345\n"), 0o644))

	pid, err := readPIDFile(path)
	require.NoError(t, err)
	assert.Equal(t, 12345, pid)
}

func TestReadPIDFile_InvalidContent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid\n"), 0o644))

	_, err := readPIDFile(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid PID")
}

func TestReadPIDFile_FileNotFound(t *testing.T) {
	t.Parallel()

	_, err := readPIDFile(filepath.Join(t.TempDir(), "nonexistent.pid"))
	assert.Error(t, err)
}

func TestSendSIGHUP_NoPIDFile(t *testing.T) {
	t.Parallel()

	err := sendSIGHUP(filepath.Join(t.TempDir(), "nonexistent.pid"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no running daemon")
}

func TestSendSIGHUP_StalePIDFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.pid")
	// PID 999999999 is almost certainly not a running process.
	require.NoError(t, os.WriteFile(path, []byte("999999999\n"), 0o644))

	err := sendSIGHUP(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not running")

	// Stale PID file should be cleaned up.
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

func TestSendSIGHUP_SendsToCurrentProcess(t *testing.T) {
	t.Parallel()

	// Trap SIGHUP so it doesn't kill the test process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	defer signal.Stop(sigCh)

	path := filepath.Join(t.TempDir(), "daemon.pid")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644))

	err := sendSIGHUP(path)
	assert.NoError(t, err)

	// Verify the signal was delivered.
	sig := <-sigCh
	assert.Equal(t, syscall.SIGHUP, sig)
}
