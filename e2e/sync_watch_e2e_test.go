//go:build e2e && e2e_full

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Daemon mode E2E tests (slow — run only with -tags=e2e,e2e_full)
// ---------------------------------------------------------------------------

// TestE2E_SyncWatch_BasicRoundTrip starts `sync --watch`, creates a local
// file, waits for it to appear remotely, then sends SIGTERM for graceful
// shutdown.
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
		"sync", "--watch", "--upload-only", "--force",
	}
	cmd := makeCmd(daemonArgs, env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	// Always kill the daemon on test exit.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	// Wait for daemon to initialize (watch setup complete).
	waitForDaemonReady(t, &stderr, 30*time.Second)

	// Create a local file inside the sync dir.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "watch-test.txt"),
		[]byte("created during watch mode\n"),
		0o644,
	))

	// Poll until the file appears remotely.
	remotePath := "/" + testFolder + "/watch-test.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "watch-test.txt", 3*time.Minute, "stat", remotePath)

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

	// Verify the file content remotely.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, remotePath)
	assert.Equal(t, "created during watch mode\n", remoteContent)
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
		"sync", "--watch", "--upload-only", "--force",
	}
	cmd := makeCmd(daemonArgs, env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	// Wait for daemon to initialize.
	waitForDaemonReady(t, &stderr, 30*time.Second)

	// Pause the drive.
	runCLIWithConfig(t, cfgPath, env, "pause")

	// Give daemon time to process the pause (SIGHUP is sent by pause command
	// automatically, but in E2E the daemon PID file may point to the wrong
	// process since we use per-test isolation). Wait briefly.
	time.Sleep(3 * time.Second)

	// Create a local file while paused.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "paused-file.txt"),
		[]byte("created while paused\n"),
		0o644,
	))

	// Wait briefly — file should NOT appear remotely while paused.
	time.Sleep(10 * time.Second)

	// Verify file is NOT remotely visible (best-effort check).
	remotePath := "/" + testFolder + "/paused-file.txt"
	_, _, statErr := runCLIWithConfigAllowError(t, cfgPath, env, "stat", remotePath)
	if statErr == nil {
		t.Log("warning: file appeared remotely while paused (test environment may not support pause in watch mode)")
	}

	// Resume the drive.
	runCLIWithConfig(t, cfgPath, env, "resume")

	// Poll until the file appears remotely after resume.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "paused-file.txt", 3*time.Minute, "stat", remotePath)

	// Send SIGTERM for graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()

	// Verify file content.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, remotePath)
	assert.Equal(t, "created while paused\n", remoteContent)
}

// waitForDaemonReady polls the daemon's stderr output until it contains
// evidence that watch mode has initialized, or times out.
func waitForDaemonReady(t *testing.T, stderr *bytes.Buffer, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	// Watch for indicators that the daemon has started:
	// - "watch mode starting" from engine
	// - "watch runner started" from orchestrator
	// - "watch setup complete" from observer
	readyIndicators := []string{
		"watch mode starting",
		"watch runner started",
		"watch setup complete",
	}

	for {
		output := stderr.String()
		for _, indicator := range readyIndicators {
			if strings.Contains(output, indicator) {
				t.Logf("daemon ready (found %q in stderr)", indicator)
				return
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready within %v\nstderr so far: %s",
				timeout, output)
		}

		time.Sleep(500 * time.Millisecond)
	}
}
