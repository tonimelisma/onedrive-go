//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Cmd        *exec.Cmd
	Stderr     *syncBuffer
	EventsPath string // structured debug-event stream; see ensureDebugEvents
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

// remoteConflictContent is the remote side of the watch-mode conflict test.
const remoteConflictContent = "remote conflict version"

// startDaemonWithStderr is like startDaemon but also returns the stderr buffer
// for tests that need to poll daemon output.
func startDaemonWithStderr(
	t *testing.T, cfgPath string, env map[string]string, args ...string,
) *daemonHandle {
	t.Helper()

	daemonArgs := []string{"--config", cfgPath, "--debug"}
	daemonArgs = append(daemonArgs, args...)

	env, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, env)

	var stdout syncBuffer
	stderr := &syncBuffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start(), "failed to start daemon")

	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, eventsPath)

	return &daemonHandle{Cmd: cmd, Stderr: stderr, EventsPath: eventsPath}
}

// TestE2E_SyncWatch_RemoteToLocal starts a download-only daemon, creates a
// file remotely, and verifies it appears locally.
func TestE2E_SyncWatch_RemoteToLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-r2l-%d", time.Now().UnixNano())

	// Start download-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--download-only", "--watch")

	// Create remote folder and file.
	mkdirRemoteFolder(t, cfgPath, env, "/"+testFolder)
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-created.txt", "from remote")

	// Poll until file appears locally.
	localPath := filepath.Join(syncDir, testFolder, "remote-created.txt")
	pollLocalFileContent(t, localPath, "from remote", daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)

	// Verify content persisted.
	data := mustReadFile(t, localPath)
	assert.Equal(t, "from remote", string(data))
}

// TestE2E_SyncWatch_Bidirectional starts a bidirectional daemon and verifies
// both local→remote and remote→local sync.
func TestE2E_SyncWatch_Bidirectional(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-bidi-%d", time.Now().UnixNano())

	// Start bidirectional daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--watch")

	// Phase 1: local → remote.
	const localContent = "from local"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "local-to-remote.txt"),
		[]byte(localContent),
		0o600,
	))

	// Poll until the file has its full size remotely, not only its name: the
	// upload creates the file empty first (LI-20261110-01).
	remotePath := "/" + testFolder + "/local-to-remote.txt"
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(localContent)), daemonPollTimeout)

	// Phase 2: remote → local.
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-to-local.txt", "from remote")

	localPath := filepath.Join(syncDir, testFolder, "remote-to-local.txt")
	pollLocalFileContent(t, localPath, "from remote", daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_ConflictDuringWatch starts a bidirectional daemon, creates
// a file, waits for it to sync, then modifies both sides to create a conflict.
// The remote is modified FIRST, by a put that has exited, then the local at
// once, so the daemon meets the local edit while its synced copy is still the
// baseline; the upload's live check finds the remote edit and the replan keeps
// both versions.
func TestE2E_SyncWatch_ConflictDuringWatch(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-conf-%d", time.Now().UnixNano())

	// Start bidirectional daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForObserverStarted(t, h.EventsPath, "local", 30*time.Second)

	// Create file and wait for it to sync.
	const baselineContent = "baseline"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "conflict-watch.txt")
	require.NoError(t, os.WriteFile(filePath, []byte(baselineContent), 0o600))

	// Wait for the upload to finish, not only for the name: the remote edit
	// below must replace the uploaded baseline, not the empty item the
	// upload creates first.
	remotePath := "/" + testFolder + "/conflict-watch.txt"
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(baselineContent)), daemonPollTimeout)

	// Modify remote FIRST: the put has exited, so the remote edit is committed
	// before the local one below.
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/conflict-watch.txt", remoteConflictContent)

	// Now modify local at once. The daemon plans the pass for it from the
	// remote truth its own poll committed, not from a delta read of its own,
	// so a short wait here would not show it the remote edit; its upload
	// checks the item's live state first, so the remote edit supersedes it and
	// the replan meets both changes. The waits below poll for that outcome.
	require.NoError(t, os.WriteFile(filePath, []byte("local conflict version"), 0o600))

	// testify runs the condition on its own goroutine, where a failing t call
	// cannot stop the test, so it only reads; the remote copy is waited for
	// after it.
	require.Eventually(t, func() bool {
		matches, globErr := globConflictCopies(filePath)
		if globErr != nil || len(matches) != 1 {
			return false
		}

		conflictCopyData, err := os.ReadFile(matches[0])
		if err != nil || string(conflictCopyData) != "local conflict version" {
			return false
		}

		localData, err := os.ReadFile(filePath) //nolint:gosec // a file inside the test's own sync dir
		return err == nil && string(localData) == remoteConflictContent
	}, daemonPollTimeout, time.Second, "watch mode should preserve both versions for edit-edit conflicts")
	waitForRemoteFileContent(t, cfgPath, env, remotePath, remoteConflictContent)

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, h.Cmd)
}

// TestE2E_SyncWatch_FileModification starts an upload-only daemon, creates
// a file, waits for upload, then modifies it and verifies the remote gets
// the new content.
//
// Uses poll_interval=30s for remote observation, and waits for
// "local observer starting watch" before creating files to eliminate the watch
// setup race.
func TestE2E_SyncWatch_FileModification(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-mod-%d", time.Now().UnixNano())

	// Start upload-only daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForObserverStarted(t, h.EventsPath, "local", 30*time.Second)

	// Create file.
	const (
		firstVersion  = "version 1"
		secondVersion = "version 2 modified"
	)
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "modifiable.txt")
	require.NoError(t, os.WriteFile(filePath, []byte(firstVersion), 0o600))

	// Wait for the initial upload to finish, not only for the name, so the
	// change below modifies an uploaded file rather than one still being
	// created.
	remotePath := "/" + testFolder + "/modifiable.txt"
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(firstVersion)), daemonPollTimeout)

	// Modify the file.
	require.NoError(t, os.WriteFile(filePath, []byte(secondVersion), 0o600))

	// Poll remote until it has the new content.
	waitForRemoteFileContent(t, cfgPath, env, remotePath, secondVersion)

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, h.Cmd)
}

// TestE2E_SyncWatch_FileDeletion starts an upload-only daemon, creates a file,
// waits for upload, then deletes it and verifies it disappears remotely.
//
// Uses poll_interval=30s for remote observation, and waits for
// "local observer starting watch" before creating files so the test does not
// race daemon bootstrap against the first watched delete.
func TestE2E_SyncWatch_FileDeletion(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir,
		"poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-del-%d", time.Now().UnixNano())

	// Start upload-only daemon with stderr access to wait for watch setup.
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Wait for fsnotify watches to be established before creating files.
	waitForObserverStarted(t, h.EventsPath, "local", 30*time.Second)

	// Create file.
	const deletedContent = "will be deleted"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "deleteme.txt")
	require.NoError(t, os.WriteFile(filePath, []byte(deletedContent), 0o600))

	// Wait for the upload to finish, not only for the name, so the delete
	// below removes an uploaded file rather than the source of an upload in
	// flight.
	remotePath := "/" + testFolder + "/deleteme.txt"
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(deletedContent)), daemonPollTimeout)

	// Delete local file.
	require.NoError(t, os.Remove(filePath))

	// Poll until file disappears from remote.
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "deleteme.txt", "ls", "/"+testFolder)

	// Graceful shutdown.
	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, h.Cmd)
}

// TestE2E_SyncWatch_FolderCreation starts an upload-only daemon and creates
// a deeply nested folder structure, verifying it appears remotely.
func TestE2E_SyncWatch_FolderCreation(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-dir-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Create deeply nested structure.
	const deepContent = "deeply nested content"
	localDir := filepath.Join(syncDir, testFolder, "a", "b", "c")
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "deep.txt"),
		[]byte(deepContent),
		0o600,
	))

	// Poll until the deep file has its full size remotely, not only its name.
	waitForRemoteStatSize(t, cfgPath, env, "/"+testFolder+"/a/b/c/deep.txt", int64(len(deepContent)), daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_MultipleFiles starts an upload-only daemon and creates
// 5 files simultaneously, verifying all appear remotely.
func TestE2E_SyncWatch_MultipleFiles(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-multi-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Create 5 files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	const (
		nameFormat    = "multi-%d.txt"
		contentFormat = "multi content %d"
	)

	for i := 1; i <= 5; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, fmt.Sprintf(nameFormat, i)),
			[]byte(fmt.Sprintf(contentFormat, i)),
			0o600,
		))
	}

	// Poll until all 5 files have their full size remotely, not only their
	// names.
	for i := 1; i <= 5; i++ {
		remotePath := "/" + testFolder + "/" + fmt.Sprintf(nameFormat, i)
		size := int64(len(fmt.Sprintf(contentFormat, i)))
		waitForRemoteStatSize(t, cfgPath, env, remotePath, size, daemonPollTimeout)
	}

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_LargeFile starts an upload-only daemon and creates a
// 5 MiB file, verifying it uploads correctly with byte-for-byte integrity.
func TestE2E_SyncWatch_LargeFile(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-large-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

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
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(fileSize), daemonPollTimeout)

	// Download and verify byte-for-byte integrity.
	downloadDir := t.TempDir()
	downloadPath := filepath.Join(downloadDir, "large-watch.bin")
	runCLIWithConfig(t, cfgPath, env, "get", remotePath, downloadPath)

	downloaded := mustReadFile(t, downloadPath)
	assert.Equal(t, data, downloaded, "downloaded content should match uploaded data")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_RapidChurn starts an upload-only daemon, rapidly modifies
// a file through several versions, and verifies the final version is synced.
func TestE2E_SyncWatch_RapidChurn(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-churn-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Create file and rapidly modify it.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "churn-watch.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("v1"), 0o600))
	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o600))
	require.NoError(t, os.WriteFile(filePath, []byte("v3"), 0o600))
	const finalVersion = "final version"
	require.NoError(t, os.WriteFile(filePath, []byte(finalVersion), 0o600))

	// Poll remote until content matches final version.
	remotePath := "/" + testFolder + "/churn-watch.txt"
	waitForRemoteFileContent(t, cfgPath, env, remotePath, finalVersion)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_GracefulShutdown verifies that after daemon shutdown,
// a subsequent one-shot sync is incremental (doesn't re-upload files that
// were already synced by the daemon).
func TestE2E_SyncWatch_GracefulShutdown(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-shut-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	// Create first file and wait for daemon to sync it.
	const daemonContent = "synced by daemon"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "before-shutdown.txt"),
		[]byte(daemonContent),
		0o600,
	))

	// Wait for the upload to finish, not only for the name: a daemon stopped
	// between the create and the content leaves the file for the one-shot
	// sync below to upload again.
	remotePath := "/" + testFolder + "/before-shutdown.txt"
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(daemonContent)), daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Logf("daemon exited with: %v", waitErr)
	}

	// Create a NEW file after daemon shutdown.
	const oneShotContent = "one-shot upload"
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "after-shutdown.txt"),
		[]byte(oneShotContent),
		0o600,
	))

	// Run one-shot sync.
	// Single pass by design: this one pass after the daemon's shutdown must
	// name the file it uploads; it may leave work a transient failure causes.
	out, _ := runSyncPass(t, cfgPath, env, "--upload-only")

	// Verify the one-shot sync uploaded only the new file (incremental).
	assert.Contains(t, out, "Uploaded ", "one-shot should name the uploaded file")

	// The one-shot pass uploaded the new file; the size wait before the
	// shutdown already holds the daemon's.
	waitForRemoteStatSize(t, cfgPath, env, "/"+testFolder+"/after-shutdown.txt", int64(len(oneShotContent)), pollTimeout)
}

const (
	// watchLocalDebounce is the quiet time a watch daemon waits after the
	// last local change before it plans a pass for it: the engine's default
	// debounce (internal/sync/engine_watch.go), which no e2e config changes.
	watchLocalDebounce = 5 * time.Second
	// pausedFileUploadAllowance is what a daemon that ignored the pause needs
	// after the debounce to put the test's small file on OneDrive where stat
	// sees it: the plan, the folder create, the file's create request and the
	// stat that then answers, with room for a slow Graph request.
	pausedFileUploadAllowance = 10 * time.Second
	// pausedUploadWindow is how long the test watches for an upload the pause
	// must hold back: a daemon that went on syncing through the pause would
	// have uploaded the file inside it.
	pausedUploadWindow = watchLocalDebounce + pausedFileUploadAllowance
	// pauseOutlastsWindowMargin covers what runs between the pause command
	// and the window's start (the reload, the local write), the window's last
	// stat with the second it sleeps afterwards, and the status command that
	// follows, so the window ends while status still reports the drive paused.
	pauseOutlastsWindowMargin = 15 * time.Second
	// timedPauseDuration is the pause the test sets and then lets expire.
	timedPauseDuration = pausedUploadWindow + pauseOutlastsWindowMargin
)

// requireNotUploadedDuringPause stats remotePath for pausedUploadWindow and
// fails the test unless every stat says the item is missing. Only that answer
// counts as "not uploaded", and only when it came before pauseEnds:
//   - a daemon whose pause ended resumes by itself, so a stat that answered
//     later cannot say what the pause held back;
//   - a lagging listing of the mount root also answers "not found" and says
//     nothing about the path, so the read recovers through the listing and a
//     listing that never answers fails the test;
//   - any other failure cannot tell either.
func requireNotUploadedDuringPause(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	remotePath string,
	pauseEnds time.Time,
) {
	t.Helper()

	driveID := resolveDriveSelection(env, "")
	windowEnds := time.Now().Add(pausedUploadWindow)
	for time.Now().Before(windowEnds) {
		_, statStderr, statErr := runRemoteReadAttemptWithRecovery(t, cfgPath, env, driveID, pauseEnds, "stat", remotePath)
		require.NotErrorIsf(t, statErr, errSharedRootListingLagging,
			"the mount root's listing did not answer, so stat cannot say whether the file uploaded\nstderr: %s",
			suiteDriveRoles.Redact(statStderr))
		require.Truef(t, time.Now().Before(pauseEnds),
			"a stat in the window answered after the pause could have ended, so it cannot say what the pause held back: lengthen pauseOutlastsWindowMargin")
		require.Error(t, statErr, "the file reached OneDrive while the drive was paused")
		require.Truef(t, isRemoteNotFoundCleanup(statStderr),
			"stat failed for a reason other than a missing item, so it cannot say whether the file uploaded\nstderr: %s",
			suiteDriveRoles.Redact(statStderr))
		sleepForLiveTestPropagation(1 * time.Second)
	}
}

// TestE2E_SyncWatch_TimedPauseExpiry starts a daemon, pauses the drive for
// timedPauseDuration, verifies nothing uploads during pausedUploadWindow
// while status reports it paused, waits until status no longer does, asks
// the daemon to reload through the control socket, and verifies the file
// written during the pause uploads.
func TestE2E_SyncWatch_TimedPauseExpiry(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-expire-%d", time.Now().UnixNano())

	// Start upload-only daemon (with stderr access for polling).
	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")
	cmd := h.Cmd

	// Pause for a while; the synchronous reload answer proves the daemon
	// applied it. The pause ends no earlier than pauseEnds: the command adds
	// the duration to its own, later clock reading and stores whole seconds.
	pauseEnds := time.Now().Add(timedPauseDuration).Truncate(time.Second)
	runCLIWithConfig(t, cfgPath, env, "pause", timedPauseDuration.String())
	reloadDaemonConfig(t, env)

	// Create local file while paused.
	const pausedContent = "created during pause"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "paused-file.txt"),
		[]byte(pausedContent),
		0o600,
	))

	// Bounded negative check: the file does not reach OneDrive during the
	// window, and status still reports the drive paused when the window ends,
	// so the window covers only time the daemon was told to hold uploads.
	remotePath := "/" + testFolder + "/paused-file.txt"
	requireNotUploadedDuringPause(t, cfgPath, env, remotePath, pauseEnds)
	require.Equal(t, statusDriveStatePaused, readStatusDriveState(t, cfgPath, env),
		"the pause ended before the window did: lengthen pauseOutlastsWindowMargin")

	// Wait until the pause has expired, then use the control socket to
	// trigger clearExpiredPauses.
	waitForStatusDriveUnpaused(t, cfgPath, env)
	reloadDaemonConfig(t, env)

	// Poll until the file has its full size remotely, not only its name
	// (daemon should now be unpaused).
	waitForRemoteStatSize(t, cfgPath, env, remotePath, int64(len(pausedContent)), daemonPollTimeout)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_SyncWatch_StatusPerfCountsRequests starts an upload-only daemon,
// lets it sync one small file, and reads `status --perf --json` while it
// runs: the watched mount's live perf snapshot counts its Graph requests.
//
// Validates: R-6.6.15
func TestE2E_SyncWatch_StatusPerfCountsRequests(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "poll_interval = \"30s\"\n")

	testFolder := fmt.Sprintf("e2e-watch-perf-%d", time.Now().UnixNano())

	// Start upload-only daemon.
	cmd := startDaemon(t, cfgPath, env,
		"--drive", resolveDriveSelection(env, ""), "sync", "--upload-only", "--watch")

	const perfContent = "counted by status --perf"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "perf-counted.txt"), []byte(perfContent), 0o600))

	// Wait for the upload to finish, not only for the name, so the daemon is
	// not stopped between the create and the content (LI-20261110-01).
	waitForRemoteStatSize(t, cfgPath, env, "/"+testFolder+"/perf-counted.txt", int64(len(perfContent)), daemonPollTimeout)

	snapshot := waitForStatusPerfRequests(t, cfgPath, env)
	assert.Positive(t, snapshot.HTTPRequestCount, "the watched mount's live perf snapshot counts its Graph requests")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}
