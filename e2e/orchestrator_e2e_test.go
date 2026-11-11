//go:build e2e && e2e_full

package e2e

import (
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
// Multi-drive orchestrator E2E tests.
//
// Build tags: e2e AND e2e_full (nightly/manual, 60-min package timeout).
// Every test skips if drive2 is empty (single-account CI, fork PRs).
// ---------------------------------------------------------------------------

func requireDrive2(t *testing.T) {
	t.Helper()

	if drive2 == "" {
		t.Skip("ONEDRIVE_TEST_DRIVE_2 not set, skipping multi-drive test")
	}
}

// TestE2E_Orchestrator_SimultaneousSync creates unique files in each drive's
// sync dir, runs sync --upload-only (no --drive flag), and verifies both
// drives' files were uploaded to their respective remotes.
func TestE2E_Orchestrator_SimultaneousSync(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	testFolder1 := fmt.Sprintf("e2e-orch-sim1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-orch-sim2-%d", time.Now().UnixNano())

	// Create local files in each drive's sync dir.
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "drive1-file.txt"), []byte("from drive1\n"), 0o600))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "drive2-file.txt"), []byte("from drive2\n"), 0o600))

	// Cleanup remote folders after test.
	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Run sync --upload-only without --drive (all drives).
	runCLIWithConfigAllDrives(t, cfgPath, env, "sync", "--upload-only")

	// Verify drive1's file exists remotely.
	remotePath1 := "/" + testFolder1 + "/drive1-file.txt"
	waitForRemoteReadContains(t, cfgPath, env, "", "drive1-file.txt", pollTimeout, "stat", remotePath1)

	// Verify drive2's file exists remotely (using drive2's --drive flag).
	remotePath2 := "/" + testFolder2 + "/drive2-file.txt"
	stdout2, _ := runCLIWithConfigForDrive(t, cfgPath, env, drive2, "stat", remotePath2)
	assert.Contains(t, stdout2, "drive2-file.txt")
}

// TestE2E_Orchestrator_Status verifies that the status command shows both
// drives when configured with a multi-drive config.
func TestE2E_Orchestrator_Status(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	stdout, _ := runCLIWithConfigAllDrives(t, cfgPath, env, "status")

	// Both drive IDs (which contain the email) should appear in status output.
	email1 := strings.SplitN(drive, ":", 2)[1]
	email2 := strings.SplitN(drive2, ":", 2)[1]
	assert.Contains(t, stdout, email1, "status should show drive1 email")
	assert.Contains(t, stdout, email2, "status should show drive2 email")
}

// TestE2E_Orchestrator_DriveIsolation uploads a file to drive1 only, then
// runs sync --download-only. Verifies the file appears in syncDir1 but NOT
// in syncDir2 (drives don't cross-pollinate).
func TestE2E_Orchestrator_DriveIsolation(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	testFolder := fmt.Sprintf("e2e-orch-iso-%d", time.Now().UnixNano())

	// Upload a file to drive1 only.
	opsCfgPath := writeMinimalConfig(t)
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/isolated.txt", "drive1 only\n")

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run download-only sync for all drives.
	runCLIWithConfigAllDrives(t, cfgPath, env, "sync", "--download-only")

	// File should appear in drive1's sync dir.
	localPath1 := filepath.Join(syncDir1, testFolder, "isolated.txt")
	_, err := os.Stat(localPath1)
	require.NoError(t, err, "file should exist in drive1 sync dir")

	// File should NOT appear in drive2's sync dir.
	localPath2 := filepath.Join(syncDir2, testFolder, "isolated.txt")
	_, err = os.Stat(localPath2)
	assert.True(t, os.IsNotExist(err), "file should NOT exist in drive2 sync dir")
}

// TestE2E_Orchestrator_OneDriveFails configures drive2 without a token file
// so session resolution fails. Verifies drive1 still succeeds while the
// overall exit is non-zero.
func TestE2E_Orchestrator_OneDriveFails(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()

	// Manually write config with both drives, but only copy drive1's token.
	// drive2's missing token causes a session error in the orchestrator.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()
	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))

	// Only copy drive1's token — deliberately omit drive2's token.
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("[%q]\nsync_dir = %q\n\n[%q]\nsync_dir = %q\n",
		drive, syncDir1, drive2, syncDir2)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	testFolder := fmt.Sprintf("e2e-orch-fail-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir1, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "survive.txt"), []byte("should survive\n"), 0o600))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run sync — should exit non-zero because drive2 fails (no token).
	stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, "sync", "--upload-only")

	// Overall should fail (one drive failed).
	require.Error(t, err, "sync should exit non-zero when one drive fails")

	// drive2's error should be mentioned.
	combined := stdout + stderr
	assert.True(t,
		strings.Contains(combined, drive2) || strings.Contains(combined, "token"),
		"output should mention the failing drive or token error")

	// Verify the CLI reported the error explicitly in the output.
	assert.Contains(t, combined, "Error:",
		"output should contain 'Error:' for the failed drive")

	// Verify drive1 reported a finished run (not stale remote state). The
	// per-drive summary is on stdout; combined holds both streams.
	assert.True(t, strings.Contains(combined, "Done in") || strings.Contains(combined, "Up to date"),
		"output should contain drive1's summary proving it completed this run")

	// drive1's file should still have been uploaded.
	remotePath := "/" + testFolder + "/survive.txt"
	waitForRemoteReadContains(t, cfgPath, env, "", "survive.txt", pollTimeout, "stat", remotePath)
}

// TestE2E_Orchestrator_SelectiveDrive creates files in both sync dirs but
// runs sync with --drive <drive1>. Verifies only drive1's file is uploaded.
func TestE2E_Orchestrator_SelectiveDrive(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	testFolder1 := fmt.Sprintf("e2e-orch-sel1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-orch-sel2-%d", time.Now().UnixNano())

	// Create files in both sync dirs.
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "selected.txt"), []byte("selected drive\n"), 0o600))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "not-selected.txt"), []byte("not selected\n"), 0o600))

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Run sync --upload-only --drive <drive1> (selective).
	runCLIWithConfigForDrive(t, cfgPath, env, drive, "sync", "--upload-only")

	// drive1's file should be uploaded.
	remotePath1 := "/" + testFolder1 + "/selected.txt"
	waitForRemoteReadContains(t, cfgPath, env, "", "selected.txt", pollTimeout, "stat", remotePath1)

	// drive2's file should NOT be uploaded (drive2 was not synced).
	// Use --drive drive2 to check drive2's remote.
	remotePath2 := "/" + testFolder2 + "/not-selected.txt"
	fullArgs := []string{"--config", cfgPath, "--drive", drive2, "--debug", "stat", remotePath2}
	cmd := makeCmd(fullArgs, env)
	statErr := cmd.Run()
	assert.Error(t, statErr, "drive2's file should not exist remotely (selective sync)")
}

// ---------------------------------------------------------------------------
// Multi-drive watch mode tests (daemon mode with multiple drives)
// ---------------------------------------------------------------------------

// TestE2E_Orchestrator_WatchSimultaneous starts a 2-drive daemon in watch
// mode, creates files in each sync dir, and verifies both are synced.
func TestE2E_Orchestrator_WatchSimultaneous(t *testing.T) {
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)
	opsCfgPath := writeMinimalConfig(t)

	testFolder1 := fmt.Sprintf("e2e-orchwatch-sim1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-orchwatch-sim2-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Start 2-drive daemon in upload-only watch mode (no --drive flag).
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

	require.NoError(t, cmd.Start(), "failed to start multi-drive daemon")

	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, eventsPath)

	// Create files in each sync dir.
	const (
		drive1Content = "watch drive1"
		drive2Content = "watch drive2"
	)
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "watch-d1.txt"), []byte(drive1Content), 0o600))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "watch-d2.txt"), []byte(drive2Content), 0o600))

	// Poll both remotes until the files have their full size, not only their
	// names: an upload creates its file empty first (LI-20261110-01).
	remotePath1 := "/" + testFolder1 + "/watch-d1.txt"
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath1, int64(len(drive1Content)), 3*time.Minute)

	remotePath2 := "/" + testFolder2 + "/watch-d2.txt"
	waitForRemoteStatSizeForDrive(t, cfgPath, env, drive2, remotePath2, int64(len(drive2Content)), 3*time.Minute)

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_Orchestrator_WatchDriveIsolation starts a 2-drive daemon, creates
// a file only in drive1's sync dir, and verifies it does NOT appear on drive2.
func TestE2E_Orchestrator_WatchDriveIsolation(t *testing.T) {
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-orchwatch-iso-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Start 2-drive daemon.
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

	waitForDaemonReady(t, eventsPath)

	// Create file only in drive1's sync dir.
	const isolatedContent = "drive1 only"
	localDir1 := filepath.Join(syncDir1, testFolder)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "isolated.txt"), []byte(isolatedContent), 0o600))

	// Wait for drive1's file to have its full size remotely, not only its name.
	remotePath := "/" + testFolder + "/isolated.txt"
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath, int64(len(isolatedContent)), 3*time.Minute)

	// Verify file does NOT appear on drive2.
	d2Args := []string{"--config", cfgPath, "--drive", drive2, "--debug", "stat", "/" + testFolder + "/isolated.txt"}
	d2Cmd := makeCmd(d2Args, env)
	d2Err := d2Cmd.Run()
	require.Error(t, d2Err, "file should NOT exist on drive2")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}

// TestE2E_Orchestrator_WatchPausedDrive starts a 2-drive daemon with drive2
// paused, creates files in both sync dirs, and verifies only drive1 syncs.
func TestE2E_Orchestrator_WatchPausedDrive(t *testing.T) {
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()

	// Create config with drive2 paused.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))

	copyTokenFile(t, testDataDir, perTestDataDir)
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, drive2)

	content := fmt.Sprintf("[%q]\nsync_dir = %q\n\n[%q]\nsync_dir = %q\npaused = true\n",
		drive, syncDir1, drive2, syncDir2)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	opsCfgPath := writeMinimalConfig(t)

	testFolder1 := fmt.Sprintf("e2e-orchwatch-pause1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-orchwatch-pause2-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Start daemon.
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

	waitForDaemonReady(t, eventsPath)

	// Create files in both sync dirs.
	const activeContent = "active drive"
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "active.txt"), []byte(activeContent), 0o600))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "paused.txt"), []byte("paused drive"), 0o600))

	// Verify drive1 syncs: the file has its full size remotely, not only its
	// name.
	remotePath1 := "/" + testFolder1 + "/active.txt"
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath1, int64(len(activeContent)), 3*time.Minute)

	// Bounded negative check: poll a few times over 10s to confirm file not on drive2.
	negDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(negDeadline) {
		d2Args := []string{"--config", cfgPath, "--drive", drive2, "--debug", "stat", "/" + testFolder2 + "/paused.txt"}
		d2Err := makeCmd(d2Args, env).Run()
		if d2Err == nil {
			t.Log("warning: paused drive2 file appeared remotely (pause may not be effective)")
			break
		}
		sleepForLiveTestPropagation(2 * time.Second)
	}
	// Final check.
	d2Args := []string{"--config", cfgPath, "--drive", drive2, "--debug", "stat", "/" + testFolder2 + "/paused.txt"}
	d2Err := makeCmd(d2Args, env).Run()
	require.Error(t, d2Err, "paused drive2's file should NOT be uploaded")

	// Graceful shutdown.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, cmd)
}
