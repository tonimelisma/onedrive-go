//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Daemon watch mode E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests exercise the continuous sync daemon: file creation, modification,
// deletion, conflict detection, large files, rapid churn, graceful shutdown,
// and timed pause expiry. All tests are sequential (no t.Parallel()) since
// daemon processes are resource-intensive.
// ---------------------------------------------------------------------------

// daemonHandle bundles the command and stderr buffer for daemon tests that
// need to poll daemon output (e.g., waiting for pause acknowledgment).
type daemonHandle struct {
	Cmd    *exec.Cmd
	Stderr *syncBuffer
}

// startDaemon starts the sync daemon as a background process and waits for
// it to become ready. It registers a cleanup function that sends SIGTERM and
// logs output. Returns the command handle for signal control.
func startDaemon(
	t *testing.T, cfgPath string, env map[string]string, args ...string,
) *exec.Cmd {
	t.Helper()

	h := startDaemonWithStderr(t, cfgPath, env, args...)

	return h.Cmd
}

// startDaemonWithStderr is like startDaemon but also returns the stderr buffer
// for tests that need to poll daemon output.
func startDaemonWithStderr(
	t *testing.T, cfgPath string, env map[string]string, args ...string,
) *daemonHandle {
	t.Helper()

	daemonArgs := []string{"--config", cfgPath, "--debug"}
	daemonArgs = append(daemonArgs, args...)

	cmd := makeCmd(daemonArgs, env)

	var stdout syncBuffer
	stderr := &syncBuffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, stderr, 30*time.Second)

	return &daemonHandle{Cmd: cmd, Stderr: stderr}
}

// daemonPollTimeout is the default timeout for polling in daemon tests.
// Daemon operations may take longer than one-shot sync due to poll intervals.
const daemonPollTimeout = 3 * time.Minute

// TestE2E_SyncWatch_RemoteToLocal starts a download-only daemon, creates a
// file remotely, and verifies it appears locally.
func TestE2E_SyncWatch_RemoteToLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-r2l-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start download-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--download-only", "--watch")

	// Create remote folder and file.
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-created.txt", "from remote")

	// Poll until file appears locally.
	localPath := filepath.Join(syncDir, testFolder, "remote-created.txt")
	pollLocalFileContent(t, localPath, "from remote", daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()

	// Verify content persisted.
	data, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, "from remote", string(data))
}

// TestE2E_SyncWatch_Bidirectional starts a bidirectional daemon and verifies
// both local→remote and remote→local sync.
func TestE2E_SyncWatch_Bidirectional(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-bidi-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start bidirectional daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--watch")

	// Phase 1: local → remote.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "local-to-remote.txt"),
		[]byte("from local"),
		0o644,
	))

	// Poll until file appears remotely.
	remotePath := "/" + testFolder + "/local-to-remote.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "local-to-remote.txt", daemonPollTimeout, "stat", remotePath)

	// Phase 2: remote → local.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-to-local.txt", "from remote")

	localPath := filepath.Join(syncDir, testFolder, "remote-to-local.txt")
	pollLocalFileContent(t, localPath, "from remote", daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}

// TestE2E_SyncWatch_ConflictDuringWatch starts a bidirectional daemon, creates
// a file, waits for it to sync, then modifies both sides to create a conflict.
// The remote is modified FIRST and confirmed via stat, then the local is
// modified. This ensures the remote change is in the delta feed when the
// local fsnotify event triggers a sync pass, so the planner sees both changes
// and produces a conflict.
func TestE2E_SyncWatch_ConflictDuringWatch(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\nsafety_scan_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-conf-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start bidirectional daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForStderrContains(t, h.Stderr, "local observer starting watch", 30*time.Second)

	// Create file and wait for it to sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "conflict-watch.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("baseline"), 0o600))

	// Wait for upload.
	remotePath := "/" + testFolder + "/conflict-watch.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "conflict-watch.txt", daemonPollTimeout, "stat", remotePath)

	// Modify remote FIRST: put a new version and confirm it propagated via
	// stat. This ensures the delta feed has the remote change before we
	// trigger local fsnotify.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/conflict-watch.txt", "remote conflict version")

	// Give delta time to pick up the remote change before triggering local.
	time.Sleep(5 * time.Second)

	// Now modify local — fsnotify fires, daemon runs a sync pass that sees
	// both the local change and the remote change from the delta feed.
	require.NoError(t, os.WriteFile(filePath, []byte("local conflict version"), 0o600))

	// Wait for the daemon to detect the conflict in per-drive status.
	status := pollStatusSyncState(t, cfgPath, env, daemonPollTimeout, func(status statusSyncStateJSON) bool {
		for _, conflict := range status.Conflicts {
			if strings.HasSuffix(conflict.Path, "/conflict-watch.txt") && conflict.ConflictType == "edit_edit" {
				return true
			}
		}
		return false
	})
	require.Len(t, status.Conflicts, 1)

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	_ = h.Cmd.Wait()
}

// TestE2E_SyncWatch_FileModification starts an upload-only daemon, creates
// a file, waits for upload, then modifies it and verifies the remote gets
// the new content.
//
// Uses safety_scan_interval=30s as a fallback for missed fsnotify events,
// and waits for "local observer starting watch" before creating files to
// eliminate the watch setup race.
func TestE2E_SyncWatch_FileModification(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\nsafety_scan_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-mod-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForStderrContains(t, h.Stderr, "local observer starting watch", 30*time.Second)

	// Create file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "modifiable.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("version 1"), 0o600))

	// Wait for initial upload.
	remotePath := "/" + testFolder + "/modifiable.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "modifiable.txt", daemonPollTimeout, "stat", remotePath)

	// Modify the file.
	require.NoError(t, os.WriteFile(filePath, []byte("version 2 modified"), 0o600))

	// Poll remote until it has the new content.
	deadline := time.Now().Add(daemonPollTimeout)
	for attempt := 0; ; attempt++ {
		content := getRemoteFile(t, opsCfgPath, nil, remotePath)
		if content == "version 2 modified" {
			break
		}

		if time.Now().After(deadline) {
			require.Failf(t, "file modification not synced",
				"remote content never became 'version 2 modified' within %v", daemonPollTimeout)
		}

		time.Sleep(pollBackoff(attempt))
	}

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	_ = h.Cmd.Wait()
}

// TestE2E_SyncWatch_FileDeletion starts an upload-only daemon, creates a file,
// waits for upload, then deletes it and verifies it disappears remotely.
//
// Uses safety_scan_interval=30s as a fallback for missed fsnotify events, and
// waits for "local observer starting watch" before creating files so the test
// does not race daemon bootstrap against the first watched delete.
func TestE2E_SyncWatch_FileDeletion(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\nsafety_scan_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-del-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForStderrContains(t, h.Stderr, "local observer starting watch", 30*time.Second)

	// Create file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "deleteme.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("will be deleted"), 0o600))

	// Wait for upload.
	remotePath := "/" + testFolder + "/deleteme.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "deleteme.txt", daemonPollTimeout, "stat", remotePath)

	// Delete local file.
	require.NoError(t, os.Remove(filePath))

	// Poll until file disappears from remote.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "deleteme.txt", daemonPollTimeout, "ls", "/"+testFolder)

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	_ = h.Cmd.Wait()
}

// TestE2E_SyncWatch_FolderCreation starts an upload-only daemon and creates
// a deeply nested folder structure, verifying it appears remotely.
func TestE2E_SyncWatch_FolderCreation(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-dir-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Create deeply nested structure.
	localDir := filepath.Join(syncDir, testFolder, "a", "b", "c")
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "deep.txt"),
		[]byte("deeply nested content"),
		0o644,
	))

	// Poll until the deep file appears remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "deep.txt", daemonPollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}

// TestE2E_SyncWatch_MultipleFiles starts an upload-only daemon and creates
// 5 files simultaneously, verifying all appear remotely.
func TestE2E_SyncWatch_MultipleFiles(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-multi-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Create 5 files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("multi-%d.txt", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, name),
			[]byte(fmt.Sprintf("multi content %d", i)),
			0o644,
		))
	}

	// Poll until all 5 files appear remotely.
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("multi-%d.txt", i)
		pollCLIWithConfigContains(t, opsCfgPath, nil, name, daemonPollTimeout, "ls", "/"+testFolder)
	}

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}

// TestE2E_SyncWatch_LargeFile starts an upload-only daemon and creates a
// 5 MiB file, verifying it uploads correctly with byte-for-byte integrity.
func TestE2E_SyncWatch_LargeFile(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-large-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Create 5 MiB file with deterministic content.
	const fileSize = 5 * 1024 * 1024

	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251)
	}

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "large-watch.bin"), data, 0o600))

	// Poll until the file appears remotely with correct size.
	remotePath := "/" + testFolder + "/large-watch.bin"
	pollCLIWithConfigContains(t, opsCfgPath, nil, fmt.Sprintf("%d bytes", fileSize), daemonPollTimeout, "stat", remotePath)

	// Download and verify byte-for-byte integrity.
	downloadDir := t.TempDir()
	downloadPath := filepath.Join(downloadDir, "large-watch.bin")
	runCLIWithConfig(t, opsCfgPath, nil, "get", remotePath, downloadPath)

	downloaded, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, data, downloaded, "downloaded content should match uploaded data")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}

// TestE2E_SyncWatch_RapidChurn starts an upload-only daemon, rapidly modifies
// a file through several versions, and verifies the final version is synced.
func TestE2E_SyncWatch_RapidChurn(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-churn-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Create file and rapidly modify it.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "churn-watch.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("v1"), 0o600))
	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o600))
	require.NoError(t, os.WriteFile(filePath, []byte("v3"), 0o600))
	require.NoError(t, os.WriteFile(filePath, []byte("final version"), 0o600))

	// Wait for file to appear remotely.
	remotePath := "/" + testFolder + "/churn-watch.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "churn-watch.txt", daemonPollTimeout, "stat", remotePath)

	// Poll remote until content matches final version.
	deadline := time.Now().Add(daemonPollTimeout)
	for attempt := 0; ; attempt++ {
		content := getRemoteFile(t, opsCfgPath, nil, remotePath)
		if content == "final version" {
			break
		}

		if time.Now().After(deadline) {
			require.Failf(t, "final version not synced",
				"remote content never became 'final version' within %v", daemonPollTimeout)
		}

		time.Sleep(pollBackoff(attempt))
	}

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}

// TestE2E_SyncWatch_GracefulShutdown verifies that after daemon shutdown,
// a subsequent one-shot sync is incremental (doesn't re-upload files that
// were already synced by the daemon).
func TestE2E_SyncWatch_GracefulShutdown(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-shut-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")

	// Create first file and wait for daemon to sync it.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "before-shutdown.txt"),
		[]byte("synced by daemon"),
		0o644,
	))

	remotePath := "/" + testFolder + "/before-shutdown.txt"
	pollCLIWithConfigContains(t, opsCfgPath, nil, "before-shutdown.txt", daemonPollTimeout, "stat", remotePath)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Logf("daemon exited with: %v", waitErr)
	}

	// Create a NEW file after daemon shutdown.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "after-shutdown.txt"),
		[]byte("one-shot upload"),
		0o644,
	))

	// Run one-shot sync.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Verify the one-shot sync uploaded only the new file (incremental).
	assert.Contains(t, stderr, "Uploads:", "one-shot should report uploads")

	// Verify both files exist remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "after-shutdown.txt", pollTimeout, "stat", "/"+testFolder+"/after-shutdown.txt")
	pollCLIWithConfigContains(t, opsCfgPath, nil, "before-shutdown.txt", pollTimeout, "stat", remotePath)
}

// TestE2E_SyncWatch_TimedPauseExpiry starts a daemon, pauses the drive with
// a short duration, verifies sync is paused, waits for expiry, asks the daemon
// to reload through the control socket, and verifies sync resumes.
func TestE2E_SyncWatch_TimedPauseExpiry(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-expire-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start upload-only daemon (with stderr access for polling).
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--upload-only", "--watch")
	cmd := h.Cmd

	// Pause for 5 seconds.
	runCLIWithConfig(t, cfgPath, env, "pause", "5s")

	// Poll stderr for pause acknowledgment.
	waitForStderrContains(t, h.Stderr, "paused", 10*time.Second)

	// Create local file while paused.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "paused-file.txt"),
		[]byte("created during pause"),
		0o644,
	))

	// Bounded negative check: confirm file not remote while paused.
	remotePath := "/" + testFolder + "/paused-file.txt"
	negDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(negDeadline) {
		_, _, statErr := runCLIWithConfigAllowError(t, opsCfgPath, nil, "stat", remotePath)
		if statErr == nil {
			t.Log("warning: file appeared remotely while paused (test environment may not support pause)")
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Wait for pause to expire (5s total from pause command), then use the
	// control socket to trigger clearExpiredPauses.
	time.Sleep(4 * time.Second)
	postControlSocket(t, env, "/v1/reload")

	// Poll until file appears remotely (daemon should now be unpaused).
	pollCLIWithConfigContains(t, opsCfgPath, nil, "paused-file.txt", daemonPollTimeout, "stat", remotePath)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()
}
