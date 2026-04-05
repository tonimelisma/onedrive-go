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
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	exe, err := os.Executable()
	require.NoError(t, err)

	//nolint:gosec // Test launches the current test binary with fixed arguments.
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRunSyncWatch_FirstSignalCancelsWatchRunner$")
	cmd.Env = append(os.Environ(), "ONEDRIVE_GO_TEST_SYNC_WATCH_HELPER=1")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start())

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "helper should announce watch startup")
	assert.Equal(t, "watch-ready", scanner.Text())

	require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGINT), "failed to send SIGINT")

	require.True(t, scanner.Scan(), "helper should announce watch cancellation")
	assert.Equal(t, "watch-canceled", scanner.Text())

	require.NoError(t, cmd.Wait(), "first signal should cancel watch runner and exit cleanly")
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
			_ synctypes.SyncMode,
			_ synctypes.WatchOpts,
			_ *slog.Logger,
			_ io.Writer,
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

func mustWriteHelperLine(line string) {
	if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
		panic(err)
	}
}

func TestSighupChannel_DeliversSignal(t *testing.T) {
	// Not parallel: sends a real SIGHUP to the process. Running in parallel
	// with other signal tests risks a window where no handler is registered
	// (between signal.Stop and signal.Notify), which terminates the process.

	ch := sighupChannel()
	defer signal.Stop(ch)

	// Send SIGHUP to ourselves.
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP), "failed to send SIGHUP")

	select {
	case sig := <-ch:
		assert.Equal(t, syscall.SIGHUP, sig)
	case <-time.After(2 * time.Second):
		require.Fail(t, "SIGHUP not received within 2 seconds")
	}
}
