package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestShutdownContext_FirstSignalCancels(t *testing.T) {
	// Not parallel: sends a real SIGINT to the process. Running in parallel
	// with other signal tests risks interference between signal handlers.

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := shutdownContext(parent, logger)

	// Send SIGINT to ourselves.
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT), "failed to send SIGINT")

	select {
	case <-ctx.Done():
		// Expected: context canceled on first signal.
	case <-time.After(2 * time.Second):
		require.Fail(t, "context not canceled within 2 seconds of SIGINT")
	}

	// Clean up: cancel parent to stop the goroutine.
	cancel()
}

func TestShutdownContext_SecondSignalForcesExit(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_TEST_SHUTDOWN_HELPER") == "1" {
		runShutdownContextSecondSignalHelper()
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	ctx := shutdownContext(parent, logger)

	// Cancel parent — derived context should also cancel.
	cancel()

	select {
	case <-ctx.Done():
		// Expected: context canceled when parent is canceled.
	case <-time.After(2 * time.Second):
		require.Fail(t, "context not canceled within 2 seconds of parent cancel")
	}
}

func runShutdownContextSecondSignalHelper() {
	logger := slog.New(slog.DiscardHandler)
	ctx := shutdownContext(context.Background(), logger)

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
		syncWatchRunner: func(
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
		},
	}

	cmd := newSyncCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
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
		{env: "XDG_CACHE_HOME", path: filepath.Join(tmpRoot, "xdg-cache")},
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
		syncDaemonOrchestratorFactory: func(_ *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			return &fakeSyncDaemonOrchestrator{}
		},
	}

	cmd := newSyncCmd()
	cmd.SetContext(context.WithValue(context.Background(), cliContextKey{}, cc))
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
