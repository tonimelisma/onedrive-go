package tokenfile

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// Validates: R-6.8.30
//
// Two handles on one sign-in file's lock exclude each other, as two
// processes do: the second waits until its context ends and says another
// process is refreshing; once the first releases, the second takes it.
func TestAcquireLock_ExcludesAnotherHandleUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens", "token_personal_x@example.com.json")

	release, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	lockInfo, statErr := os.Stat(path + LockSuffix)
	require.NoError(t, statErr)
	assert.Equal(t, fs.FileMode(FilePerms), lockInfo.Mode().Perm())

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	noop, err := AcquireLock(ended, path)
	require.ErrorIs(t, err, ErrLockWaitEnded)
	require.NoError(t, noop())

	require.NoError(t, release())
	again, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, again())
}

// Validates: R-6.8.30
//
// A purge removes the lock file; a missing one is no error.
func TestRemoveLock_RemovesTheLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token_personal_x@example.com.json")
	release, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, release())

	require.NoError(t, RemoveLock(path))
	_, statErr := os.Stat(path + LockSuffix)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	require.NoError(t, RemoveLock(path))
}

// Validates: R-6.8.30
//
// The undo of a login whose context has ended still waits for a refresh
// holding the lock and then puts back the sign-in the install replaced.
func TestInstall_UndoAfterTheLoginEndedWaitsForTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token_personal_x@example.com.json")
	pending := filepath.Join(dir, "pending.json")
	previous := &oauth2.Token{AccessToken: "previous-access", RefreshToken: "previous-refresh"}
	require.NoError(t, Save(path, previous))
	require.NoError(t, Save(pending, &oauth2.Token{AccessToken: "new-access", RefreshToken: "new-refresh"}))

	login, cancel := context.WithCancel(t.Context())
	undo, err := Install(login, pending, path)
	require.NoError(t, err)

	release, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	cancel()
	released := make(chan error, 1)
	timer := time.AfterFunc(200*time.Millisecond, func() { released <- release() })
	t.Cleanup(func() { timer.Stop() })

	require.NoError(t, undo())
	require.NoError(t, <-released)
	restored, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, previous.RefreshToken, restored.RefreshToken)
}

// lockHolderEnv makes the test binary, started by
// TestAcquireLock_ExcludesAnotherProcess, hold the lock of the sign-in file
// it names until its standard input closes.
const lockHolderEnv = "ONEDRIVE_GO_TEST_SIGN_IN_LOCK_HOLDER"

// Validates: R-6.8.30
//
// The lock holds between processes: while a second process holds it, this
// one cannot take it; once that process exits, it can.
func TestAcquireLock_ExcludesAnotherProcess(t *testing.T) {
	if holderPath := os.Getenv(lockHolderEnv); holderPath != "" {
		holdLockUntilStdinCloses(t, holderPath)
		return
	}

	path := filepath.Join(t.TempDir(), "token_personal_x@example.com.json")
	//nolint:gosec // the test binary re-runs itself with a fixed test name
	holder := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAcquireLock_ExcludesAnotherProcess$")
	holder.Env = append(os.Environ(), lockHolderEnv+"="+path)
	stdin, err := holder.StdinPipe()
	require.NoError(t, err)
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = AcquireLock(ended, path)
	require.ErrorIs(t, err, ErrLockWaitEnded, "held by the other process")

	require.NoError(t, stdin.Close())
	require.NoError(t, holder.Wait())
	release, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, release())
}

func holdLockUntilStdinCloses(t *testing.T, path string) {
	t.Helper()

	release, err := AcquireLock(t.Context(), path)
	require.NoError(t, err)
	_, err = os.Stdout.WriteString("locked\n")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
	require.NoError(t, release())
}
