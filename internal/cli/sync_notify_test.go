package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/sdnotify"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const watchdogMessage = "WATCHDOG=1"

// fakeNotifySocket plays systemd's side of $NOTIFY_SOCKET.
type fakeNotifySocket struct {
	t       *testing.T
	address string
	conn    *net.UnixConn
}

func newFakeNotifySocket(t *testing.T) *fakeNotifySocket {
	t.Helper()

	address := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	return &fakeNotifySocket{t: t, address: address, conn: conn}
}

func (f *fakeNotifySocket) next() string {
	f.t.Helper()

	require.NoError(f.t, f.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 4096)
	n, err := f.conn.Read(buf)
	require.NoError(f.t, err)

	return string(buf[:n])
}

// nextNonWatchdog skips keepalives, which arrive on their own schedule.
func (f *fakeNotifySocket) nextNonWatchdog() string {
	f.t.Helper()

	for {
		if message := f.next(); message != watchdogMessage {
			return message
		}
	}
}

func (f *fakeNotifySocket) notifier(env map[string]string) *sdnotify.Notifier {
	f.t.Helper()

	env[sdnotify.EnvNotifySocket] = f.address
	notifier, err := sdnotify.Open(func(key string) string { return env[key] }, 1)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { assert.NoError(f.t, notifier.Close()) })

	return notifier
}

// lifecycleOrchestrator drives the WatchLifecycle hook the way RunWatch does.
type lifecycleOrchestrator struct {
	cfg     *multisync.OrchestratorConfig
	events  []multisync.WatchLifecycleEvent
	release <-chan struct{}
	err     error
}

func (o *lifecycleOrchestrator) RunWatch(context.Context, syncengine.SyncMode, syncengine.WatchOptions) error {
	for _, event := range o.events {
		o.cfg.WatchLifecycle(event)
	}
	if o.release != nil {
		<-o.release
	}

	return o.err
}

func writeNotifyTestConfig(t *testing.T) (*config.Holder, string) {
	t.Helper()

	setTestDriveHome(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	syncDir := t.TempDir()
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))

	return loadSyncTestHolder(t, cfgPath), syncDir
}

// Validates: R-6.6.32
func TestRunSyncDaemon_NotifiesSystemdOfLifecycle(t *testing.T) {
	holder, syncDir := writeNotifyTestConfig(t)
	socket := newFakeNotifySocket(t)
	// 2ms timeout: a keepalive every millisecond, so one arrives while the
	// fake watch is running.
	notifier := socket.notifier(map[string]string{sdnotify.EnvWatchdogUSec: "2000"})

	release := make(chan struct{})
	orch := &lifecycleOrchestrator{release: release, events: []multisync.WatchLifecycleEvent{
		{Phase: multisync.WatchReady, SyncRoots: []string{syncDir}},
		{Phase: multisync.WatchReloading},
		{Phase: multisync.WatchReloaded, SyncRoots: []string{syncDir}},
		{Phase: multisync.WatchReloading},
		{Phase: multisync.WatchReloaded, Err: errors.New("bad toml")},
		{Phase: multisync.WatchStopping},
	}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runSyncDaemonWithFactory(t.Context(), holder, nil, syncengine.SyncBidirectional,
			syncengine.WatchOptions{}, slog.New(slog.DiscardHandler), io.Discard, "/tmp/control.sock",
			nil, notifier, func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
				orch.cfg = cfg
				return orch
			})
	}()

	assert.Equal(t, "READY=1\nSTATUS=Watching "+syncDir, socket.nextNonWatchdog())
	reloading := socket.nextNonWatchdog()
	assert.True(t, strings.HasPrefix(reloading, "RELOADING=1\nMONOTONIC_USEC="), reloading)
	assert.True(t, strings.HasSuffix(reloading, "\nSTATUS=Reloading the config"), reloading)
	assert.Equal(t, "READY=1\nSTATUS=Watching "+syncDir, socket.nextNonWatchdog())
	assert.True(t, strings.HasPrefix(socket.nextNonWatchdog(), "RELOADING=1\n"))
	assert.Equal(t,
		"READY=1\nSTATUS=Watching "+syncDir+"; couldn't reload the config, kept the previous settings",
		socket.nextNonWatchdog())
	assert.Equal(t, "STOPPING=1\nSTATUS=Stopping", socket.nextNonWatchdog())

	// Keepalives keep coming while the watch runs.
	keepaliveSeen := false
	for !keepaliveSeen {
		keepaliveSeen = socket.next() == watchdogMessage
	}
	close(release)

	require.NoError(t, <-errCh)
}

// Validates: R-6.6.32, R-6.6.31
func TestRunSyncDaemon_SignInFailureSetsStatusAndExitCode(t *testing.T) {
	holder, _ := writeNotifyTestConfig(t)
	socket := newFakeNotifySocket(t)
	notifier := socket.notifier(map[string]string{})

	cid := driveid.MustCanonicalID("personal:user@example.com")
	orch := &lifecycleOrchestrator{err: &multisync.WatchStartupError{
		Summary: multisync.StartupSelectionSummary{Results: []multisync.MountStartupResult{{
			Identity: testStandaloneMountIdentity(cid),
			Status:   multisync.MountStartupFatal,
			Err:      fmt.Errorf("session: %w", graph.ErrNotLoggedIn),
		}}},
	}}

	err := runSyncDaemonWithFactory(t.Context(), holder, nil, syncengine.SyncBidirectional,
		syncengine.WatchOptions{}, slog.New(slog.DiscardHandler), io.Discard, "/tmp/control.sock",
		nil, notifier, func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			orch.cfg = cfg
			return orch
		})

	require.Error(t, err)
	assert.Equal(t, exitSignInRequired, commandExitCode(err, false))
	assert.Equal(t, "STATUS=Sign-in required: run 'onedrive-go login'", socket.next())
}

// Validates: R-6.6.32, R-6.6.31
func TestRunSyncDaemon_NoDrivesIsAConfigFailure(t *testing.T) {
	t.Parallel()

	socket := newFakeNotifySocket(t)
	notifier := socket.notifier(map[string]string{})

	err := runSyncDaemonWithFactory(t.Context(), config.NewHolder(&config.Config{}, ""), nil,
		syncengine.SyncBidirectional, syncengine.WatchOptions{}, slog.New(slog.DiscardHandler), io.Discard,
		"/tmp/control.sock", nil, notifier, nil)

	require.Error(t, err)
	assert.Equal(t, exitConfigInvalid, commandExitCode(err, false))
	assert.Equal(t,
		"STATUS=Invalid config: no drives configured — run 'onedrive-go drive add' to add a drive",
		socket.next())
}

// Validates: R-6.6.32
func TestSyncNotifierWatchingStatus(t *testing.T) {
	t.Parallel()

	s := newSyncNotifier(nil, slog.New(slog.DiscardHandler), &syncConsole{home: "/home/user"})
	signIn := multisync.MountStartupResult{
		DisplayName: "Work",
		Status:      multisync.MountStartupFatal,
		Err:         fmt.Errorf("token: %w", graph.ErrUnauthorized),
	}
	other := multisync.MountStartupResult{DisplayName: "Old", Status: multisync.MountStartupFatal, Err: errors.New("boom")}

	tests := []struct {
		name  string
		event multisync.WatchLifecycleEvent
		want  string
	}{
		{
			name:  "one drive under home",
			event: multisync.WatchLifecycleEvent{SyncRoots: []string{"/home/user/OneDrive"}},
			want:  "Watching ~/OneDrive",
		},
		{
			name:  "several drives",
			event: multisync.WatchLifecycleEvent{SyncRoots: []string{"/data/a", "/home/user/b"}},
			want:  "Watching /data/a, ~/b",
		},
		{
			name:  "many drives are counted",
			event: multisync.WatchLifecycleEvent{SyncRoots: []string{"/a", "/b", "/c", "/d"}},
			want:  "Watching 4 drives",
		},
		{
			name: "sign-in required and other failures",
			event: multisync.WatchLifecycleEvent{
				SyncRoots: []string{"/home/user/OneDrive"},
				Skipped:   []multisync.MountStartupResult{signIn, other},
			},
			want: "Watching ~/OneDrive; sign-in required for Work; 1 drive could not start",
		},
		{
			name:  "nothing running",
			event: multisync.WatchLifecycleEvent{Skipped: []multisync.MountStartupResult{other, other}},
			want:  "Watching no drives; 2 drives could not start",
		},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, s.watchingStatus(tt.event), tt.name)
	}
}

// Validates: R-6.6.32
func TestOpenSyncNotifier_BadSocketWarnsAndContinues(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	notifier := openSyncNotifier(func(key string) string {
		if key == sdnotify.EnvNotifySocket {
			return "relative/socket"
		}
		return ""
	}, logger)

	assert.Nil(t, notifier)
	assert.Contains(t, logs.String(), "notify socket")
	assert.Nil(t, openSyncNotifier(func(string) string { return "" }, logger))
}
