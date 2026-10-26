package multisync

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// shortSocketDir keeps the socket path under the Unix socket length limit.
func shortSocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "odgo-lk")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })

	return dir
}

// Validates: R-2.9.4
func TestStartControlSocketServer_RacingOwnersYieldExactlyOne(t *testing.T) {
	t.Parallel()

	for range 20 {
		dir := shortSocketDir(t)
		socketPath := filepath.Join(dir, "control.sock")
		// A leftover socket file from a sync that crashed: both contenders
		// fail to dial it, which is when the old code let each remove the
		// other's fresh socket.
		require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0o600))

		const contenders = 2
		servers := make([]*controlSocketServer, contenders)
		errs := make([]error, contenders)
		start := make(chan struct{})
		var wg gosync.WaitGroup
		for i := range contenders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				servers[i], errs[i] = startControlSocketServer(t.Context(), socketPath, http.NewServeMux(), synctest.TestLogger(t))
			}()
		}
		close(start)
		wg.Wait()

		winners := 0
		for i := range contenders {
			if errs[i] == nil {
				winners++
				continue
			}
			var inUse *ControlSocketInUseError
			require.ErrorAs(t, errs[i], &inUse, "the loser is told a sync is running")
		}
		require.Equal(t, 1, winners, "exactly one owner")

		var dialer net.Dialer
		conn, dialErr := dialer.DialContext(t.Context(), "unix", socketPath)
		require.NoError(t, dialErr, "the winner's socket survives the race")
		require.NoError(t, conn.Close())

		for i := range contenders {
			if servers[i] != nil {
				require.NoError(t, servers[i].Close(t.Context()))
			}
		}
	}
}

// Validates: R-2.9.4
func TestStartControlSocketServer_LockReleasedOnCloseAndOnlyThen(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "control.sock")
	first, err := startControlSocketServer(t.Context(), socketPath, http.NewServeMux(), synctest.TestLogger(t))
	require.NoError(t, err)

	_, err = startControlSocketServer(t.Context(), socketPath, http.NewServeMux(), synctest.TestLogger(t))
	var inUse *ControlSocketInUseError
	require.ErrorAs(t, err, &inUse)
	assert.FileExists(t, socketPath, "a refused contender must not touch the owner's socket")

	require.NoError(t, first.Close(t.Context()))
	assert.NoFileExists(t, socketPath+controlLockSuffix)

	second, err := startControlSocketServer(t.Context(), socketPath, http.NewServeMux(), synctest.TestLogger(t))
	require.NoError(t, err)
	require.NoError(t, second.Close(t.Context()))
}

// Validates: R-2.9.4
func TestLockFileStillAtPath_DetectsAnUnlinkedOrReplacedLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "control.sock.lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, file.Close()) })

	same, err := lockFileStillAtPath(file, path)
	require.NoError(t, err)
	assert.True(t, same)

	require.NoError(t, os.Remove(path))
	same, err = lockFileStillAtPath(file, path)
	require.NoError(t, err)
	assert.False(t, same, "a lock on an unlinked file proves nothing")

	require.NoError(t, os.WriteFile(path, nil, 0o600))
	same, err = lockFileStillAtPath(file, path)
	require.NoError(t, err)
	assert.False(t, same, "a lock on a replaced file proves nothing")
}
