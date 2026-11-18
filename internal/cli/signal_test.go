package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graphtransport"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func TestShutdownContext_FirstSignalCancels(t *testing.T) {
	// Not parallel: sends a real SIGINT to the process. Running in parallel
	// with other signal tests risks interference between signal handlers.

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, stop := shutdownContext(parent, logger)
	defer stop()

	// Send SIGINT to ourselves.
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT), "failed to send SIGINT")

	receiveWithin(t, ctx.Done(), "the first SIGINT to cancel the context")

	// Clean up: cancel parent to stop the goroutine.
	cancel()
}

func TestShutdownContext_SecondSignalForcesExit(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_TEST_SHUTDOWN_HELPER") == "1" {
		runShutdownContextSecondSignalHelper()
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()

	exe, err := os.Executable()
	require.NoError(t, err)

	//nolint:gosec // Test launches the current test binary with fixed arguments.
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestShutdownContext_SecondSignalForcesExit$")
	cmd.Env = append(os.Environ(), "ONEDRIVE_GO_TEST_SHUTDOWN_HELPER=1")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start())

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "helper should announce readiness")
	assert.Equal(t, "ready", scanner.Text())

	require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGINT), "failed to send first SIGINT")

	require.True(t, scanner.Scan(), "helper should acknowledge graceful shutdown")
	assert.Equal(t, "canceled", scanner.Text())

	require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGINT), "failed to send second SIGINT")

	err = cmd.Wait()
	require.Error(t, err, "second signal should force a non-zero exit")

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.NoError(t, scanner.Err())
}

// Validates: R-2.8.3
func TestRunSyncWatch_FirstSignalCancelsWatchRunner(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_TEST_SYNC_WATCH_HELPER") == "1" {
		runSyncWatchFirstSignalHelper()
		return
	}

	runFirstSignalSyncWatchSubprocess(
		t,
		"ONEDRIVE_GO_TEST_SYNC_WATCH_HELPER",
		"^TestRunSyncWatch_FirstSignalCancelsWatchRunner$",
		"watch-ready",
		"watch-canceled",
		"first signal should cancel watch runner and exit cleanly",
	)
}

// Validates: R-2.8.3
func TestRunSyncWatch_FirstSignalCancelsDaemonOrchestrator(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_TEST_SYNC_DAEMON_HELPER") == "1" {
		runSyncWatchDaemonFirstSignalHelper()
		return
	}

	runFirstSignalSyncWatchSubprocess(
		t,
		"ONEDRIVE_GO_TEST_SYNC_DAEMON_HELPER",
		"^TestRunSyncWatch_FirstSignalCancelsDaemonOrchestrator$",
		"watch-daemon-ready",
		"watch-daemon-canceled",
		"first signal should cancel the watch daemon and exit cleanly",
	)
}

func runFirstSignalSyncWatchSubprocess(
	t *testing.T,
	helperEnv string,
	testPattern string,
	readyLine string,
	canceledLine string,
	waitMessage string,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()

	exe, err := os.Executable()
	require.NoError(t, err)

	//nolint:gosec // Test launches the current test binary with fixed arguments.
	cmd := exec.CommandContext(ctx, exe, "-test.run="+testPattern)
	cmd.Env = append(os.Environ(), helperEnv+"=1")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start())

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "helper should announce watch startup")
	assert.Equal(t, readyLine, scanner.Text())

	require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGINT), "failed to send SIGINT")

	require.True(t, scanner.Scan(), "helper should announce watch cancellation")
	assert.Equal(t, canceledLine, scanner.Text())

	require.NoError(t, cmd.Wait(), waitMessage)
	assert.NoError(t, scanner.Err())
}

func TestShutdownContext_ParentCancelStopsGoroutine(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(t.Context())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, stop := shutdownContext(parent, logger)
	defer stop()

	// Cancel parent — derived context should also cancel.
	cancel()

	receiveWithin(t, ctx.Done(), "the parent's cancel to cancel the derived context")
}

func runShutdownContextSecondSignalHelper() {
	logger := slog.New(slog.DiscardHandler)
	ctx, stop := shutdownContext(context.Background(), logger)
	defer stop()

	mustWriteHelperLine("ready")
	<-ctx.Done()
	mustWriteHelperLine("canceled")

	select {}
}

func runSyncWatchFirstSignalHelper() {
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      filepath.Join(os.TempDir(), "onedrive-go-sync-watch-helper.toml"),
		seams: cliSeams{syncWatch: func(
			ctx context.Context,
			_ *config.Holder,
			_ []string,
			_ syncengine.SyncMode,
			_ syncengine.WatchOptions,
			_ *slog.Logger,
			_ io.Writer,
			_ string,
		) error {
			mustWriteHelperLine("watch-ready")
			<-ctx.Done()
			mustWriteHelperLine("watch-canceled")
			return nil
		}},
	}

	cmd := newSyncCmd()
	// Mirror the real entry point, which installs signal handling once for
	// every command rather than inside runSync.
	signalCtx, stopSignals := shutdownContext(context.Background(), slog.New(slog.DiscardHandler))
	defer stopSignals()

	cmd.SetContext(context.WithValue(signalCtx, cliContextKey{}, cc))
	if err := cmd.Flags().Set("watch", "true"); err != nil {
		panic(err)
	}
	if err := runSync(cmd, nil); err != nil {
		panic(err)
	}
}

type fakeSyncDaemonOrchestrator struct{}

func (o *fakeSyncDaemonOrchestrator) RunWatch(
	ctx context.Context,
	_ syncengine.SyncMode,
	_ syncengine.WatchOptions,
) error {
	mustWriteHelperLine("watch-daemon-ready")
	<-ctx.Done()
	mustWriteHelperLine("watch-daemon-canceled")
	return nil
}

func runSyncWatchDaemonFirstSignalHelper() {
	tmpRoot, err := os.MkdirTemp("", "onedrive-go-sync-daemon-helper")
	if err != nil {
		panic(err)
	}

	paths := []struct {
		env  string
		path string
	}{
		{env: "HOME", path: filepath.Join(tmpRoot, "home")},
		{env: "XDG_CONFIG_HOME", path: filepath.Join(tmpRoot, "xdg-config")},
		{env: "XDG_DATA_HOME", path: filepath.Join(tmpRoot, "xdg-data")},
	}
	for i := range paths {
		if err := os.MkdirAll(paths[i].path, 0o750); err != nil {
			panic(err)
		}
		if err := os.Setenv(paths[i].env, paths[i].path); err != nil {
			panic(err)
		}
	}

	syncDir := filepath.Join(tmpRoot, "sync-root")
	if err := os.MkdirAll(syncDir, 0o750); err != nil {
		panic(err)
	}

	cfgPath := filepath.Join(tmpRoot, "config.toml")
	configBody := fmt.Sprintf(`
["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	if err := os.WriteFile(cfgPath, []byte(configBody), 0o600); err != nil {
		panic(err)
	}

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
		seams: cliSeams{
			syncDaemonOrchestrator: func(_ *multisync.OrchestratorConfig) syncDaemonOrchestrator {
				return &fakeSyncDaemonOrchestrator{}
			},
		},
	}

	cmd := newSyncCmd()
	// Mirror the real entry point, which installs signal handling once for
	// every command rather than inside runSync.
	signalCtx, stopSignals := shutdownContext(context.Background(), slog.New(slog.DiscardHandler))
	defer stopSignals()

	cmd.SetContext(context.WithValue(signalCtx, cliContextKey{}, cc))
	if err := cmd.Flags().Set("watch", "true"); err != nil {
		panic(err)
	}
	if err := runSync(cmd, nil); err != nil {
		panic(err)
	}
}

func mustWriteHelperLine(line string) {
	if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
		panic(err)
	}
}

// Validates: R-6.3.6
//
// Signal handling is installed once per command invocation rather than once
// per process, so the returned cancel has to release the goroutine and
// unregister the handler. Without that, every invocation in a long-lived
// process -- a test binary running the CLI repeatedly, most obviously --
// would strand a goroutine and leave another handler registered.
func TestShutdownContext_CancelReleasesSignalGoroutine(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	before := runtime.NumGoroutine()

	for range 50 {
		_, stop := shutdownContext(context.Background(), logger)
		stop()
	}

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+2
	}, eventWait, 10*time.Millisecond,
		"signal goroutines should unwind after cancel")
}

// Validates: R-6.3.6
//
// After a first signal the goroutine waits for a second one. The command
// context is already canceled by then and the parent is context.Background()
// at the real entry point, so only the stop call can release it; before this
// test, a command interrupted once left its goroutine and its signal
// registration behind for the rest of the process.
func TestShutdownContext_StopAfterFirstSignalReleasesSignalGoroutine(t *testing.T) {
	// Not parallel: sends a real SIGINT to the process.

	logger := slog.New(slog.DiscardHandler)
	before := runtime.NumGoroutine()

	ctx, stop := shutdownContext(context.Background(), logger)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT), "failed to send SIGINT")
	receiveWithin(t, ctx.Done(), "the first SIGINT to cancel the context")

	stop()

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+2
	}, eventWait, 10*time.Millisecond,
		"the signal goroutine must unwind once stop is called after a first signal")
}

// A panic in the signal handler cancels the command with the panic as the
// cause, so the command ends and exits 1, not 130: the user's stop is not
// what ended it. Unrecovered, it would end the process with Go's exit
// status 2.
//
// Validates: R-6.3.10, R-6.3.6
func TestShutdownContext_HandlerPanicCancelsTheCommandWithTheError(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	logger := slog.New(synctest.PanicOnMessage{
		Message: "Stopping... (press Ctrl-C again to force quit)",
		Next:    slog.DiscardHandler,
	})
	ctx, stop := shutdownContext(context.Background(), logger)
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT), "failed to send SIGINT")
	receiveWithin(t, ctx.Done(), "the handler's panic to end the command")

	require.ErrorIs(t, commandPanicCause(ctx), panicguard.ErrPanic)
	require.ErrorContains(t, commandPanicCause(ctx), "internal error in the signal handler: log call exploded")
	assert.Equal(t, exitFailure, commandExitCode(withCommandPanic(ctx, ctx.Err()), true))
}

// A request the handler's panic interrupts carries the panic, the cause of
// the cancel, in place of context.Canceled. It reads as stopped, as under any
// other stop, and the command still ends with the panic and exits 1.
//
// Validates: R-6.3.10
func TestShutdownContext_HandlerPanicStopsTheRequestInFlight(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	logger := slog.New(synctest.PanicOnMessage{
		Message: "Stopping... (press Ctrl-C again to force quit)",
		Next:    slog.DiscardHandler,
	})
	ctx, stop := shutdownContext(context.Background(), logger)
	defer stop()

	// The server stops the command once the request has reached it, and
	// never answers.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	client := graphtransport.InteractiveMetadataClient(slog.New(slog.DiscardHandler), nil, nil)
	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}

	require.ErrorIs(t, err, context.Canceled, "the interrupted request reads as stopped")
	require.ErrorIs(t, err, panicguard.ErrPanic, "and still carries the panic")
	assert.Equal(t, errclass.ClassFatal, classifyCommandError(withCommandPanic(ctx, err)))
	assert.Equal(t, exitFailure, commandExitCode(withCommandPanic(ctx, err), true))
}
