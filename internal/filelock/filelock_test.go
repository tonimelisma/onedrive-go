package filelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPoll is the retry interval of the tests' waits.
const testPoll = time.Millisecond

// openLockFile opens the lock file at path as another process would: its
// own open file description, closed when the test ends.
func openLockFile(t *testing.T, path string) *os.File {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // a test's own temporary lock file
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, file.Close()) })

	return file
}

// Validates: R-4.4.4, R-2.9.5
//
// Two descriptors of one file exclude each other, as two processes do; an
// unlock lets the other take it.
func TestTryLock_ExcludesAnotherHandleUntilUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, second := openLockFile(t, path), openLockFile(t, path)

	locked, err := TryLock(nil, int(first.Fd()))
	require.NoError(t, err)
	require.True(t, locked)

	locked, err = TryLock(nil, int(second.Fd()))
	require.NoError(t, err)
	assert.False(t, locked, "held by the other handle")

	require.NoError(t, Unlock(nil, int(first.Fd())))
	locked, err = TryLock(nil, int(second.Fd()))
	require.NoError(t, err)
	assert.True(t, locked, "released")
}

// Validates: R-4.4.4, R-2.9.5
//
// A wait ends with its context while the lock stays held, and takes the lock
// once the holder releases it.
func TestWait_EndsWithTheContextAndTakesAReleasedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	holder, waiter := openLockFile(t, path), openLockFile(t, path)
	locked, err := TryLock(nil, int(holder.Fd()))
	require.NoError(t, err)
	require.True(t, locked)

	cause := errors.New("test: gave up")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	require.ErrorIs(t, Wait(ctx, nil, int(waiter.Fd()), testPoll), cause)

	attempts := 0
	releasing := Flock(func(fd, how int) error {
		attempts++
		if attempts == 2 {
			require.NoError(t, Unlock(nil, int(holder.Fd())))
		}

		return syscall.Flock(fd, how)
	})
	require.NoError(t, Wait(t.Context(), releasing, int(waiter.Fd()), testPoll))
	assert.Equal(t, 2, attempts, "held once, then taken after the release")
}

// Validates: R-4.4.4, R-2.9.5
func TestTryLock_ReturnsOtherFailuresForClassification(t *testing.T) {
	unsupported := Flock(func(int, int) error { return syscall.ENOLCK })

	locked, err := TryLock(unsupported, 0)
	assert.False(t, locked)
	require.ErrorIs(t, err, syscall.ENOLCK)
	assert.True(t, IsUnsupported(err))
	assert.False(t, IsUnsupported(syscall.EIO))
	require.ErrorIs(t, Wait(t.Context(), unsupported, 0, testPoll), syscall.ENOLCK, "a wait does not retry a failure")
}
