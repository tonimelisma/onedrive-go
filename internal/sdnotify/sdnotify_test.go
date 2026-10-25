package sdnotify

import (
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeManager is a stand-in for systemd's notify socket.
type fakeManager struct {
	t    *testing.T
	conn *net.UnixConn
}

func newFakeManager(t *testing.T, address string) *fakeManager {
	t.Helper()

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	return &fakeManager{t: t, conn: conn}
}

func (m *fakeManager) next() string {
	m.t.Helper()

	require.NoError(m.t, m.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 4096)
	n, err := m.conn.Read(buf)
	require.NoError(m.t, err)

	return string(buf[:n])
}

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func openFake(t *testing.T, env map[string]string) (*Notifier, *fakeManager) {
	t.Helper()

	address := filepath.Join(t.TempDir(), "notify.sock")
	manager := newFakeManager(t, address)
	env[EnvNotifySocket] = address

	notifier, err := Open(envOf(env), 4242)
	require.NoError(t, err)
	require.NotNil(t, notifier)
	t.Cleanup(func() { assert.NoError(t, notifier.Close()) })

	return notifier, manager
}

// Validates: R-6.6.32
func TestOpen_UnsetSocketIsANoOpNotifier(t *testing.T) {
	t.Parallel()

	notifier, err := Open(envOf(map[string]string{EnvWatchdogUSec: "1000000"}), 1)
	require.NoError(t, err)
	assert.Nil(t, notifier)

	// Every method is safe on the nil notifier.
	assert.NoError(t, notifier.Ready("x"))
	assert.NoError(t, notifier.Status("x"))
	assert.NoError(t, notifier.Reloading("x"))
	assert.NoError(t, notifier.Stopping("x"))
	assert.NoError(t, notifier.Watchdog())
	assert.Zero(t, notifier.WatchdogInterval())
	assert.NoError(t, notifier.Close())
}

// Validates: R-6.6.32
func TestNotifier_SendsProtocolMessages(t *testing.T) {
	t.Parallel()

	notifier, manager := openFake(t, map[string]string{})
	notifier.monotonic = func() (time.Duration, error) { return 1234567 * time.Microsecond, nil }

	require.NoError(t, notifier.Ready("Watching ~/OneDrive"))
	assert.Equal(t, "READY=1\nSTATUS=Watching ~/OneDrive", manager.next())

	require.NoError(t, notifier.Status("Sign-in required\nfor user@example.com"))
	assert.Equal(t, "STATUS=Sign-in required for user@example.com", manager.next(),
		"a status is always one line")

	require.NoError(t, notifier.Reloading("Reloading the config"))
	assert.Equal(t, "RELOADING=1\nMONOTONIC_USEC=1234567\nSTATUS=Reloading the config", manager.next())

	require.NoError(t, notifier.Stopping(""))
	assert.Equal(t, "STOPPING=1", manager.next())

	require.NoError(t, notifier.Watchdog())
	assert.Equal(t, "WATCHDOG=1", manager.next())
}

// Validates: R-6.6.32
func TestNotifier_RealMonotonicClock(t *testing.T) {
	t.Parallel()

	notifier, manager := openFake(t, map[string]string{})
	require.NoError(t, notifier.Reloading(""))

	var usec int64
	_, err := fmt.Sscanf(manager.next(), "RELOADING=1\nMONOTONIC_USEC=%d", &usec)
	require.NoError(t, err)
	assert.Positive(t, usec)
}

// Validates: R-6.6.32
func TestOpen_AbstractSocket(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("abstract unix sockets are Linux-only")
	}

	address := "@onedrive-go-sdnotify-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	manager := newFakeManager(t, address)

	notifier, err := Open(envOf(map[string]string{EnvNotifySocket: address}), 1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, notifier.Close()) })

	require.NoError(t, notifier.Ready(""))
	assert.Equal(t, "READY=1", manager.next())
}

// Validates: R-6.6.32
func TestOpen_RejectsUnsupportedAndUnreachableSockets(t *testing.T) {
	t.Parallel()

	_, err := Open(envOf(map[string]string{EnvNotifySocket: "vsock:2:1234"}), 1)
	require.ErrorIs(t, err, ErrUnsupportedSocket)

	_, err = Open(envOf(map[string]string{EnvNotifySocket: filepath.Join(t.TempDir(), "missing.sock")}), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sdnotify: connect")
}

// Validates: R-6.6.32
func TestOpen_WatchdogInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    time.Duration
		wantErr bool
	}{
		{name: "no watchdog", env: map[string]string{}, want: 0},
		{name: "half the timeout", env: map[string]string{EnvWatchdogUSec: "30000000"}, want: 15 * time.Second},
		{
			name: "for this process",
			env:  map[string]string{EnvWatchdogUSec: "2000000", EnvWatchdogPID: "4242"},
			want: time.Second,
		},
		{
			name: "for another process",
			env:  map[string]string{EnvWatchdogUSec: "2000000", EnvWatchdogPID: "1"},
			want: 0,
		},
		{name: "bad timeout", env: map[string]string{EnvWatchdogUSec: "soon"}, wantErr: true},
		{name: "zero timeout", env: map[string]string{EnvWatchdogUSec: "0"}, wantErr: true},
		{name: "bad pid", env: map[string]string{EnvWatchdogUSec: "1000000", EnvWatchdogPID: "me"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			address := filepath.Join(t.TempDir(), "n.sock")
			newFakeManager(t, address)
			tt.env[EnvNotifySocket] = address

			notifier, err := Open(envOf(tt.env), 4242)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, notifier.Close()) })
			assert.Equal(t, tt.want, notifier.WatchdogInterval())
		})
	}
}

// Validates: R-6.6.32
func TestNotifier_SendAfterCloseFails(t *testing.T) {
	t.Parallel()

	address := filepath.Join(t.TempDir(), "n.sock")
	newFakeManager(t, address)
	notifier, err := Open(envOf(map[string]string{EnvNotifySocket: address}), 1)
	require.NoError(t, err)
	require.NoError(t, notifier.Close())

	assert.Error(t, notifier.Ready(""))
}
