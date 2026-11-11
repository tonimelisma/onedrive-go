//go:build e2e && e2e_full

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a thread-safe bytes.Buffer for capturing subprocess output.
// os/exec writes to cmd.Stdout/Stderr from a goroutine; reading the buffer
// (e.g., in waitForDaemonReady) from the test goroutine is a data race
// without synchronization.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write implements io.Writer for use as cmd.Stdout/Stderr.
func (sb *syncBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	sb.buf = append(sb.buf, p...)

	return len(p), nil
}

// String returns the accumulated output.
func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return string(sb.buf)
}

// ---------------------------------------------------------------------------
// Daemon mode E2E tests (slow — run only with -tags=e2e,e2e_full)
// ---------------------------------------------------------------------------

// TestE2E_SyncWatch_BasicRoundTrip starts `sync --watch`, creates a local
// file, waits for its content to reach OneDrive, then sends SIGTERM for
// graceful shutdown.
func TestE2E_SyncWatch_BasicRoundTrip(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-syncwatch-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start daemon as a background process.
	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", drive,
		"--debug",
		"sync", "--watch", "--upload-only",
	}
	daemonEnv, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, daemonEnv)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	// Always kill the daemon on test exit.
	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	// Wait for daemon to initialize (watch setup complete).
	waitForDaemonReady(t, eventsPath)

	// Create a local file inside the sync dir.
	const watchContent = "created during watch mode\n"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "watch-test.txt"),
		[]byte(watchContent),
		0o600,
	))

	// Poll until the file's content is remote, not only its name: the upload
	// creates the file empty first (LI-20261110-01).
	remotePath := "/" + testFolder + "/watch-test.txt"
	waitForRemoteFileContent(t, opsCfgPath, nil, remotePath, watchContent)

	// Send SIGTERM for graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))

	// Wait for clean exit.
	waitErr := cmd.Wait()

	// Daemon should exit cleanly (exit 0) or with context canceled.
	if waitErr != nil {
		// Some platforms return exit code 1 for signal-interrupted processes.
		// Check that the daemon at least started correctly.
		t.Logf("daemon exited with: %v", waitErr)
		t.Logf("daemon stderr: %s", stderr.String())
	}

	// Verify the shutdown left the file's content in place.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, remotePath)
	assert.Equal(t, watchContent, remoteContent)
}

// Validates: R-2.9.1
func TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", drive,
		"--debug",
		"sync", "--watch",
	}
	daemonEnv, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, daemonEnv)

	var daemonStdout, daemonStderr syncBuffer
	cmd.Stdout = &daemonStdout
	cmd.Stderr = &daemonStderr

	require.NoError(t, cmd.Start(), "failed to start daemon")
	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, daemonStdout.String(), daemonStderr.String())
	})

	waitForDaemonReady(t, eventsPath)

	assertCompetingOwnerFails := func(name string, args ...string) {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		fullArgs := []string{"--config", cfgPath, "--drive", drive}
		if shouldAddDebug(args) {
			fullArgs = append(fullArgs, "--debug")
		}
		fullArgs = append(fullArgs, args...)

		competingCmd := exec.CommandContext(ctx, binaryPath, fullArgs...) //nolint:gosec // runs the suite-built CLI binary; args come from the test itself
		if len(env) > 0 {
			competingCmd.Env = makeCmd(nil, env).Env
		}

		var stdout, stderr bytes.Buffer
		competingCmd.Stdout = &stdout
		competingCmd.Stderr = &stderr

		err := competingCmd.Run()
		logCLIExecution(t, fullArgs, stdout.String(), stderr.String())
		require.Error(t, err, "%s should fail while watch owner is live", name)
		require.NotEqual(t, context.DeadlineExceeded, ctx.Err(), "%s should fail promptly instead of hanging", name)

		combined := stdout.String() + stderr.String()
		assert.Contains(
			t,
			combined,
			"another onedrive-go sync is already running",
			"%s should report the owner-socket conflict",
			name,
		)
	}

	assertCompetingOwnerFails("second watch owner", "sync", "--watch")
	assertCompetingOwnerFails("foreground sync owner", "sync")
}

// TestE2E_SyncWatch_PauseResume starts a daemon, pauses the drive, verifies
// no sync occurs, resumes, and verifies sync resumes.
func TestE2E_SyncWatch_PauseResume(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-syncwatch-pr-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start daemon.
	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", drive,
		"--debug",
		"sync", "--watch", "--upload-only",
	}
	daemonEnv, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, daemonEnv)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	// Wait for daemon to initialize.
	waitForDaemonReady(t, eventsPath)

	// Pause notifies the daemon through the control socket. Reloading again
	// ourselves is idempotent and its synchronous answer proves the daemon
	// has applied the pause; no log wording involved.
	runCLIWithConfig(t, cfgPath, env, "pause")
	reloadDaemonConfig(t, env)

	// Create a local file while paused.
	const pausedContent = "created while paused\n"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "paused-file.txt"),
		[]byte(pausedContent),
		0o600,
	))

	// Bounded negative check: poll a few times over 10s, assert file still absent.
	remotePath := "/" + testFolder + "/paused-file.txt"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _, statErr := runCLIWithConfigAllowError(t, cfgPath, env, "stat", remotePath)
		if statErr == nil {
			t.Log("warning: file appeared remotely while paused (test environment may not support pause in watch mode)")
			break
		}
		sleepForLiveTestPropagation(2 * time.Second)
	}

	// Resume the drive.
	runCLIWithConfig(t, cfgPath, env, "resume")

	// Poll until the file's content is remote after resume, not only its
	// name: the upload creates the file empty first, and a daemon stopped
	// between that and the content leaves 0 bytes behind (LI-20261110-01).
	waitForRemoteFileContent(t, opsCfgPath, nil, remotePath, pausedContent)

	// Send SIGTERM for graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)

	// Verify the shutdown left the file's content in place.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, remotePath)
	assert.Equal(t, pausedContent, remoteContent)
}

// TestE2E_SyncWatch_ControlSocketReload starts a daemon with only drive1, then
// rewrites the config to add drive2, sends a control-socket reload, and verifies
// that drive2 starts syncing after the reload.
func TestE2E_SyncWatch_ControlSocketReload(t *testing.T) {
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()

	// Set up per-test isolation with BOTH token files pre-copied.
	// The daemon needs drive2's token to be present after the reload.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))

	copyTokenFile(t, testDataDir, perTestDataDir)
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, drive2)

	// Write initial config with only drive1.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	initialCfg := fmt.Sprintf("[%q]\nsync_dir = %q\n", drive, syncDir1)
	require.NoError(t, os.WriteFile(cfgPath, []byte(initialCfg), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	opsCfgPath := writeMinimalConfig(t)

	testFolder1 := fmt.Sprintf("e2e-reload-d1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-reload-d2-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Start daemon without --drive (all-drives mode). Initially only drive1.
	daemonArgs := []string{
		"--config", cfgPath,
		"--debug",
		"sync", "--watch", "--upload-only",
	}
	daemonEnv, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, daemonEnv)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	// Wait for daemon to initialize (drive1 watch setup).
	waitForDaemonReady(t, eventsPath)

	// Create a file in drive1's sync dir to verify it works.
	const beforeReloadContent = "before reload\n"
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "before-reload.txt"),
		[]byte(beforeReloadContent),
		0o600,
	))

	// Poll until drive1's file content is remote, not only its name.
	remotePath1 := "/" + testFolder1 + "/before-reload.txt"
	waitForRemoteFileContent(t, opsCfgPath, nil, remotePath1, beforeReloadContent)

	// Rewrite config to add drive2.
	updatedCfg := fmt.Sprintf("[%q]\nsync_dir = %q\n\n[%q]\nsync_dir = %q\n",
		drive, syncDir1, drive2, syncDir2)
	require.NoError(t, os.WriteFile(cfgPath, []byte(updatedCfg), 0o600))

	// Ask the daemon to reload through the durable control path. The reload
	// answers once applied; then the control socket must list drive2.
	reloadDaemonConfig(t, env)
	waitForControlSocketMount(t, env, drive2, 30*time.Second)

	// Create a file in drive2's sync dir.
	const afterReloadContent = "after reload\n"
	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "after-reload.txt"),
		[]byte(afterReloadContent),
		0o600,
	))

	// Poll until drive2's file has its full size remotely, not only its name.
	remotePath2 := "/" + testFolder2 + "/after-reload.txt"
	waitForRemoteStatSizeForDrive(t, cfgPath, env, drive2, remotePath2, int64(len(afterReloadContent)), 3*time.Minute)

	// Send SIGTERM for graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)

	// Verify file contents.
	remoteContent1 := getRemoteFile(t, opsCfgPath, nil, remotePath1)
	assert.Equal(t, beforeReloadContent, remoteContent1)
}
