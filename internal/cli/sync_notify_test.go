package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sdnotify"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
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

	address := shortSocketPath(t, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	return &fakeNotifySocket{t: t, address: address, conn: conn}
}

func (f *fakeNotifySocket) next() string {
	f.t.Helper()

	require.NoError(f.t, f.conn.SetReadDeadline(time.Now().Add(eventWait)))
	buf := make([]byte, 4096)
	n, err := f.conn.Read(buf)
	require.NoError(f.t, err, "no notify message arrived")

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
		{Phase: multisync.WatchReloaded, Reload: multisync.ReloadApplied, SyncRoots: []string{syncDir}},
		{Phase: multisync.WatchReloading},
		{Phase: multisync.WatchReloaded, Reload: multisync.ReloadKeptPreviousSettings, Err: errors.New("bad toml")},
		{Phase: multisync.WatchStopping},
	}}

	done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		return runSyncDaemonWithFactory(ctx, holder, nil, syncengine.SyncBidirectional,
			syncengine.WatchOptions{}, slog.New(slog.DiscardHandler), io.Discard, "/tmp/control.sock",
			nil, notifier, func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
				orch.cfg = cfg
				return orch
			})
	})
	// The fake watch ignores its context and ends when release closes; a
	// test that fails first closes it here. Registered after the run's own
	// cleanup, so it runs first.
	endWatch := sync.OnceFunc(func() { close(release) })
	t.Cleanup(endWatch)

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
	endWatch()

	require.NoError(t, receiveWithin(t, done, "the sync daemon to return after the watch ended"))
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

// A watch whose loop panicked ends with the panic: the final STATUS= says
// why, and the exit status is 1.
//
// Validates: R-6.3.10, R-6.6.32
func TestRunSyncDaemon_LoopPanicSetsTheFailedStatusAndExitsOne(t *testing.T) {
	holder, _ := writeNotifyTestConfig(t)
	socket := newFakeNotifySocket(t)
	notifier := socket.notifier(map[string]string{})

	orch := &lifecycleOrchestrator{err: panicguard.Recovered(slog.New(slog.DiscardHandler), "in the watch loop", "boom")}
	err := runSyncDaemonWithFactory(t.Context(), holder, nil, syncengine.SyncBidirectional,
		syncengine.WatchOptions{}, slog.New(slog.DiscardHandler), io.Discard, "/tmp/control.sock",
		nil, notifier, func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			orch.cfg = cfg
			return orch
		})

	require.ErrorIs(t, err, panicguard.ErrPanic)
	assert.Equal(t, exitFailure, commandExitCode(err, false))
	assert.Equal(t,
		"STATUS=Failed: internal error in the watch loop: boom. This is a bug; please report it, with the log file if you have one",
		socket.next())
}

// contextBoundOrchestrator is a watch that runs until its context ends.
type contextBoundOrchestrator struct{}

func (contextBoundOrchestrator) RunWatch(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
	<-ctx.Done()
	return nil
}

// runWatchWithPanickingKeepalive runs a watch of orch under a unit with
// WatchdogSec= whose keepalive panics, and returns the watch's error. With
// nobody listening on the notify socket every keepalive fails, and the
// failure's log call is the one that panics.
func runWatchWithPanickingKeepalive(t *testing.T, orch syncDaemonOrchestrator) error {
	t.Helper()

	holder, _ := writeNotifyTestConfig(t)
	address := shortSocketPath(t, "notify.sock")
	receiver, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	require.NoError(t, err)
	env := map[string]string{sdnotify.EnvNotifySocket: address, sdnotify.EnvWatchdogUSec: "2000"}
	notifier, err := sdnotify.Open(func(key string) string { return env[key] }, 1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, notifier.Close()) })
	require.NoError(t, receiver.Close())
	logger := slog.New(synctest.PanicOnMessage{Message: "systemd watchdog keepalive failed", Next: slog.DiscardHandler})

	return runWithin(t, t.Context(), "the keepalive's panic to end the watch", func(ctx context.Context) error {
		return runSyncDaemonWithFactory(ctx, holder, nil, syncengine.SyncBidirectional,
			syncengine.WatchOptions{}, logger, io.Discard, "/tmp/control.sock",
			nil, notifier, func(*multisync.OrchestratorConfig) syncDaemonOrchestrator {
				return orch
			})
	})
}

// A panic in the systemd keepalive ends the watch through its context, with
// the panic as the cause: the watch drains as on a stop and returns the
// panic, so the command exits 1.
//
// Validates: R-6.3.10, R-6.6.32
func TestRunSyncDaemon_WatchdogPanicEndsTheWatchWithItsError(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	err := runWatchWithPanickingKeepalive(t, contextBoundOrchestrator{})

	require.ErrorIs(t, err, panicguard.ErrPanic)
	require.ErrorContains(t, err, "internal error in the systemd watchdog keepalive: log call exploded")
	assert.Equal(t, exitFailure, commandExitCode(err, false))
}

// causeReadingOrchestrator is a watch that runs until its context ends and
// then hands back what work that end interrupted would: its context's cause.
type causeReadingOrchestrator struct{}

func (causeReadingOrchestrator) RunWatch(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
	<-ctx.Done()
	return fmt.Errorf("request canceled: %w", context.Cause(ctx))
}

// What the keepalive's panic interrupts in the watch reads as stopped, as
// under any other stop, so the drives drain as on a stop; the watch still
// ends with the panic and exits 1.
//
// Validates: R-6.3.10, R-6.8.29
func TestRunSyncDaemon_WatchdogPanicStopsTheWorkInFlight(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	err := runWatchWithPanickingKeepalive(t, causeReadingOrchestrator{})

	require.ErrorIs(t, err, context.Canceled, "the interrupted work reads as stopped")
	require.ErrorIs(t, err, panicguard.ErrPanic, "and still carries the panic")
	assert.Equal(t, exitFailure, commandExitCode(err, false))
}

// errWatchDrainFailed is a failure of its own a watch ends with while it
// stops, such as a drain that did not finish.
var errWatchDrainFailed = errors.New("the drain did not finish")

// failingStopOrchestrator is a watch that runs until its context ends and
// then fails on its way out with a failure of its own.
type failingStopOrchestrator struct{}

func (failingStopOrchestrator) RunWatch(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
	<-ctx.Done()
	return errWatchDrainFailed
}

// A watch the keepalive's panic stopped, that then fails on its way out, ends
// with both: the panic, which only this function's context carries, and the
// failure. The command exits 1 and prints the panic.
//
// Validates: R-6.3.10, R-6.6.32
func TestRunSyncDaemon_WatchdogPanicJoinsTheFailureTheWatchEndsWith(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	err := runWatchWithPanickingKeepalive(t, failingStopOrchestrator{})

	require.ErrorIs(t, err, panicguard.ErrPanic, "the panic is not lost behind the watch's own failure")
	require.ErrorIs(t, err, errWatchDrainFailed)
	assert.Equal(t, exitFailure, commandExitCode(err, false))
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
	paused := multisync.MountStartupResult{DisplayName: "Home", Status: multisync.MountStartupPaused}

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
		{
			name:  "every drive paused idles rather than fails",
			event: multisync.WatchLifecycleEvent{Paused: []multisync.MountStartupResult{paused, paused}},
			want:  "All drives are paused; run 'onedrive-go resume' to continue",
		},
		{
			name: "a paused drive beside a running one",
			event: multisync.WatchLifecycleEvent{
				SyncRoots: []string{"/home/user/OneDrive"},
				Paused:    []multisync.MountStartupResult{paused},
			},
			want: "Watching ~/OneDrive; 1 drive paused",
		},
		{
			name: "paused and failed drives with nothing running",
			event: multisync.WatchLifecycleEvent{
				Skipped: []multisync.MountStartupResult{other},
				Paused:  []multisync.MountStartupResult{paused},
			},
			want: "Watching no drives; 1 drive could not start; 1 drive paused",
		},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, s.watchingStatus(tt.event), tt.name)
	}
}

// The status after a reload follows the outcome the watch states. An applied
// reload that also carries an error (no such reload exists today) is not
// reported as one that kept the previous settings, which is what reading the
// outcome from Err did.
//
// Validates: R-6.6.32, R-6.3.9
func TestSyncNotifier_ReloadStatusFollowsTheStatedOutcome(t *testing.T) {
	socket := newFakeNotifySocket(t)
	s := newSyncNotifier(socket.notifier(map[string]string{}), slog.New(slog.DiscardHandler), nil)
	s.readyStatus = "Watching /old"

	s.lifecycle(multisync.WatchLifecycleEvent{
		Phase:     multisync.WatchReloaded,
		Reload:    multisync.ReloadApplied,
		SyncRoots: []string{"/new"},
		Err:       errors.New("not a reason to keep anything"),
	})
	assert.Equal(t, "READY=1\nSTATUS=Watching /new", socket.next())

	s.lifecycle(multisync.WatchLifecycleEvent{
		Phase:  multisync.WatchReloaded,
		Reload: multisync.ReloadKeptPreviousSettings,
		Err:    errors.New("bad toml"),
	})
	assert.Equal(t, "READY=1\nSTATUS=Watching /new; couldn't reload the config, kept the previous settings", socket.next())
}

// Validates: R-6.8.26, R-6.6.32
//
// While a refreshed sign-in cannot be saved, every status of the ready watch
// names its file after what the watch says today; once the save succeeds,
// the status is the watch's own again.
func TestSyncNotifier_StatusNamesAnUnsavedSignInWhileItIsSet(t *testing.T) {
	socket := newFakeNotifySocket(t)
	s := newSyncNotifier(socket.notifier(map[string]string{}), slog.New(slog.DiscardHandler), nil)

	s.lifecycle(multisync.WatchLifecycleEvent{Phase: multisync.WatchReady, SyncRoots: []string{"/sync"}})
	assert.Equal(t, "READY=1\nSTATUS=Watching /sync", socket.next())

	s.lifecycle(multisync.WatchLifecycleEvent{
		Phase:          multisync.WatchSignInSaveChanged,
		UnsavedSignIns: []string{"/tokens/sign-in.json"},
	})
	assert.Equal(t, "STATUS=Watching /sync; couldn't save the refreshed sign-in to /tokens/sign-in.json", socket.next())

	s.lifecycle(multisync.WatchLifecycleEvent{
		Phase: multisync.WatchReloaded, Reload: multisync.ReloadApplied, SyncRoots: []string{"/sync", "/other"},
	})
	assert.Equal(t,
		"READY=1\nSTATUS=Watching /sync, /other; couldn't save the refreshed sign-in to /tokens/sign-in.json",
		socket.next(), "a reload's status keeps naming it")

	s.lifecycle(multisync.WatchLifecycleEvent{Phase: multisync.WatchSignInSaveChanged})
	assert.Equal(t, "STATUS=Watching /sync, /other", socket.next())
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

// shortSocketPath returns a socket path under /tmp. macOS limits a Unix socket
// path to 104 bytes, and its per-test TMPDIR under /var/folders is too long.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "odg-sd")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })

	return filepath.Join(dir, name)
}
