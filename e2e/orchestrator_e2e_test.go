//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Multi-drive orchestrator E2E tests.
//
// Build tags: e2e AND e2e_full (nightly/manual, 30-min timeout).
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
	require.NoError(t, os.MkdirAll(localDir1, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "drive1-file.txt"), []byte("from drive1\n"), 0o644))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "drive2-file.txt"), []byte("from drive2\n"), 0o644))

	// Cleanup remote folders after test.
	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Run sync --upload-only --force without --drive (all drives).
	runCLIWithConfigAllDrives(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify drive1's file exists remotely.
	remotePath1 := "/" + testFolder1 + "/drive1-file.txt"
	pollCLIWithConfigContains(t, cfgPath, env, "drive1-file.txt", pollTimeout, "stat", remotePath1)

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
	runCLIWithConfigAllDrives(t, cfgPath, env, "sync", "--download-only", "--force")

	// File should appear in drive1's sync dir.
	localPath1 := filepath.Join(syncDir1, testFolder, "isolated.txt")
	_, err := os.Stat(localPath1)
	assert.NoError(t, err, "file should exist in drive1 sync dir")

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
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o755))

	// Only copy drive1's token — deliberately omit drive2's token.
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("[%q]\nsync_dir = %q\n\n[%q]\nsync_dir = %q\n",
		drive, syncDir1, drive2, syncDir2)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	testFolder := fmt.Sprintf("e2e-orch-fail-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir1, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "survive.txt"), []byte("should survive\n"), 0o644))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run sync — should exit non-zero because drive2 fails (no token).
	stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Overall should fail (one drive failed).
	assert.Error(t, err, "sync should exit non-zero when one drive fails")

	// drive2's error should be mentioned.
	combined := stdout + stderr
	assert.True(t,
		strings.Contains(combined, drive2) || strings.Contains(combined, "token"),
		"output should mention the failing drive or token error")

	// Verify the CLI reported the error explicitly in the output.
	assert.Contains(t, combined, "Error:",
		"output should contain 'Error:' for the failed drive")

	// Verify drive1 reported success in this run (not stale remote state).
	assert.Contains(t, stdout, "Succeeded:",
		"stdout should contain 'Succeeded:' proving drive1 completed this run")

	// drive1's file should still have been uploaded.
	remotePath := "/" + testFolder + "/survive.txt"
	pollCLIWithConfigContains(t, cfgPath, env, "survive.txt", pollTimeout, "stat", remotePath)
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
	require.NoError(t, os.MkdirAll(localDir1, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "selected.txt"), []byte("selected drive\n"), 0o644))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "not-selected.txt"), []byte("not selected\n"), 0o644))

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Run sync --upload-only --drive <drive1> (selective).
	runCLIWithConfigForDrive(t, cfgPath, env, drive, "sync", "--upload-only", "--force")

	// drive1's file should be uploaded.
	remotePath1 := "/" + testFolder1 + "/selected.txt"
	pollCLIWithConfigContains(t, cfgPath, env, "selected.txt", pollTimeout, "stat", remotePath1)

	// drive2's file should NOT be uploaded (drive2 was not synced).
	// Use --drive drive2 to check drive2's remote.
	remotePath2 := "/" + testFolder2 + "/not-selected.txt"
	fullArgs := []string{"--config", cfgPath, "--drive", drive2, "--debug", "stat", remotePath2}
	cmd := makeCmd(fullArgs, env)
	statErr := cmd.Run()
	assert.Error(t, statErr, "drive2's file should not exist remotely (selective sync)")
}
