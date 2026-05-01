//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
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
// CLI command E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests validate the status, pause, resume, and sync CLI UX against a
// live OneDrive account.
// ---------------------------------------------------------------------------

// TestE2E_Status_AfterSync validates that status shows token state and last
// sync information after a successful sync.
func TestE2E_Status_AfterSync(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-status-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "status-test.txt"), []byte("status test"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Check status output.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "Auth:", "status should show auth state")
	assert.Contains(t, stdout, "ready", "status should show ready state after sync")
}

// TestE2E_Status_JSON validates that status --json exposes the unified
// summary-plus-per-mount schema.
func TestE2E_Status_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	status := readStatus(t, cfgPath, env)
	assert.Equal(t, 1, status.Summary.TotalMounts)
	mountStatus := requireStatusMount(t, status, drive)
	require.NotNil(t, mountStatus.SyncState)
	assert.Equal(t, 5, mountStatus.SyncState.ExamplesLimit)
	assert.False(t, mountStatus.SyncState.Verbose)
}

// TestE2E_Status_PausedDrive validates that pausing a drive changes its
// status to "paused" and resuming changes it back to "ready".
func TestE2E_Status_PausedDrive(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Pause the drive.
	runCLIWithConfig(t, cfgPath, env, "pause")

	// Check status shows "paused".
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "paused", "status should show paused state")

	// Resume the drive.
	runCLIWithConfig(t, cfgPath, env, "resume")

	// Check status shows "ready".
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "ready", "status should show ready state after resume")
}

// TestE2E_Pause_WithDuration validates that pausing with a duration shows
// a "paused until" message with an RFC3339 timestamp.
func TestE2E_Pause_WithDuration(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Pause for 30 seconds.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "pause", "30s")
	assert.Contains(t, stderr, "paused until", "pause with duration should show 'paused until'")

	// Resume to clean up.
	runCLIWithConfig(t, cfgPath, env, "resume")
}

// TestE2E_Pause_IndefiniteAndResume validates the full pause/resume lifecycle
// for an indefinite pause.
func TestE2E_Pause_IndefiniteAndResume(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Pause indefinitely.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "pause")
	assert.Contains(t, stderr, "paused", "pause should confirm drive is paused")

	// Resume.
	_, stderr = runCLIWithConfig(t, cfgPath, env, "resume")
	assert.Contains(t, stderr, "resumed", "resume should confirm drive is resumed")

	// Status should show ready.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "ready", "status should show ready after resume")
}

// TestE2E_Resume_NotPaused validates that resuming a non-paused drive gives
// an appropriate message.
func TestE2E_Resume_NotPaused(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Resume on a non-paused drive.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "resume")
	assert.Contains(t, stderr, "is not paused",
		"resuming a non-paused drive should say 'is not paused'")
}

// TestE2E_Resume_AllDrives validates that resuming without --drive resumes
// all paused drives. Requires drive2.
func TestE2E_Resume_AllDrives(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	// Pause both drives.
	runCLIWithConfigForDrive(t, cfgPath, env, drive, "pause")
	runCLIWithConfigForDrive(t, cfgPath, env, drive2, "pause")

	// Resume all drives (no --drive flag).
	_, stderr := runCLIWithConfigAllDrives(t, cfgPath, env, "resume")
	assert.Contains(t, stderr, "resumed", "resume all should confirm drives resumed")
}

// Validates: R-2.3.3
func TestE2E_Status_PerDrive_NoConditionsOrRetries(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-noconditions-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state DB.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "clean.txt"), []byte("clean file"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, status.Conditions)
	assert.Zero(t, status.ConditionCount)
	assert.Zero(t, status.Retrying)
}

// Validates: R-2.3.4
// TestE2E_Status_NoLegacyHistorySurface validates that `status` stays the only
// sync-health surface and rejects the removed `--history` flag.
func TestE2E_Status_NoLegacyHistorySurface(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-noconfl-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "clean.txt"), []byte("clean file"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	current := readStatusSyncState(t, cfgPath, env)
	assert.Zero(t, current.ConditionCount, "clean status should show no durable conditions")
	assert.Empty(t, current.Conditions)

	output := runCLIWithConfigExpectError(t, cfgPath, env, "status", "--history")
	assert.Contains(t, output, "unknown flag: --history", "status should expose the current condition view only")
}

// Validates: R-2.3.4, R-2.3.10
// TestE2E_Status_JSON_ConditionDetails validates that per-mount status JSON
// exposes the current structured condition payload.
func TestE2E_Status_JSON_ConditionDetails(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-condjson-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "desktop.ini"), []byte("reserved"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatusSyncState(t, cfgPath, env)
	require.Len(t, status.Conditions, 1, "status should surface one invalid-filename condition for the reserved name")
	assert.Equal(t, 1, status.ConditionCount)

	condition := status.Conditions[0]
	assert.Equal(t, "invalid_filename", condition.ConditionKey)
	assert.Equal(t, "invalid_filename", condition.ConditionType)
	assert.Equal(t, 1, condition.Count)
	assert.NotEmpty(t, condition.Reason)
	assert.NotEmpty(t, condition.Action)
	assert.Contains(t, strings.Join(condition.Paths, "\n"), filepath.ToSlash(filepath.Join(testFolder, "desktop.ini")))
}

// Validates: R-2.3.12
// TestE2E_CLI_NoResolveCommand validates that sync exposes no out-of-band
// command for replaying planner/executor decisions.
func TestE2E_CLI_NoResolveCommand(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-notfound-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state DB.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "dummy.txt"), []byte("dummy"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	output := runCLIWithConfigExpectError(t, cfgPath, env, "resolve", "local", "nonexistent-id")
	assert.Contains(t, output, "unknown command \"resolve\"", "sync decisions should stay inside the engine path")
}

// TestE2E_InternalBaselineVerification_AfterSync validates that the internal
// baseline verifier reports a clean tree after sync.
func TestE2E_InternalBaselineVerification_AfterSync(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-verify-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 3 files and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v1.txt"), []byte("verify 1"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v2.txt"), []byte("verify 2"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v3.txt"), []byte("verify 3"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_RecycleBinRoundtrip validates the full recycle bin workflow:
// put → rm → recycle-bin list → recycle-bin restore → verify restored.
func TestE2E_RecycleBinRoundtrip(t *testing.T) {
	t.Parallel()
	skipIfPersonalDrive(t)
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-recycle-bin-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create remote folder and upload a file.
	localFile := filepath.Join(syncDir, "recycle-test.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("recycle bin test content"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "mkdir", "/"+testFolder)
	runCLIWithConfig(t, cfgPath, env, "put", localFile, "/"+testFolder+"/recycle-test.txt")

	// Get the item ID via ls --json.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder, "--json")
	var lsItems []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &lsItems),
		"ls --json should produce valid JSON, got: %s", stdout)
	require.Len(t, lsItems, 1)
	itemID, ok := lsItems[0]["id"].(string)
	require.True(t, ok, "item should have an id field")

	// Delete the file (moves to recycle bin).
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/recycle-test.txt")

	// List recycle bin — should contain our file.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "recycle-bin", "list")
	assert.Contains(t, stdout, "recycle-test.txt", "recycle bin should contain deleted file")

	// List in JSON mode.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "recycle-bin", "list", "--json")
	var rbItems []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &rbItems),
		"recycle-bin list --json should produce valid JSON, got: %s", stdout)

	// Find our item in the list.
	var foundID string
	for _, item := range rbItems {
		if name, nameOK := item["name"].(string); nameOK && name == "recycle-test.txt" {
			foundID, _ = item["id"].(string)
		}
	}
	require.NotEmpty(t, foundID, "should find recycle-test.txt in recycle bin list")
	assert.Equal(t, itemID, foundID, "recycle bin item ID should match original")

	// Restore the file.
	runCLIWithConfig(t, cfgPath, env, "recycle-bin", "restore", itemID)

	// Verify the file is back.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder, "--json")
	require.NoError(t, json.Unmarshal([]byte(stdout), &lsItems),
		"ls --json after restore should produce valid JSON, got: %s", stdout)
	require.Len(t, lsItems, 1)
	assert.Equal(t, "recycle-test.txt", lsItems[0]["name"])
}

// TestE2E_RecycleBinEmpty validates that recycle-bin empty --confirm
// permanently removes items from the recycle bin.
func TestE2E_RecycleBinEmpty(t *testing.T) {
	t.Parallel()
	skipIfPersonalDrive(t)
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-recycle-empty-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create remote folder, upload, and delete a file to put it in the recycle bin.
	localFile := filepath.Join(syncDir, "empty-test.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("will be permanently deleted"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "mkdir", "/"+testFolder)
	runCLIWithConfig(t, cfgPath, env, "put", localFile, "/"+testFolder+"/empty-test.txt")
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/empty-test.txt")

	// Verify the item is in the recycle bin.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "recycle-bin", "list")
	assert.Contains(t, stdout, "empty-test.txt", "file should be in recycle bin")

	// Empty without --confirm should fail.
	_, _, err := runCLIWithConfigAllowError(t, cfgPath, env, "recycle-bin", "empty")
	require.Error(t, err, "empty without --confirm should fail")

	// Empty with --confirm should succeed.
	runCLIWithConfig(t, cfgPath, env, "recycle-bin", "empty", "--confirm")

	// Verify the recycle bin is empty or the file is gone.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "recycle-bin", "list")
	assert.NotContains(t, stdout, "empty-test.txt",
		"file should no longer be in recycle bin after empty")
}

// ---------------------------------------------------------------------------
// mv E2E tests
// ---------------------------------------------------------------------------

// Validates: R-1.6
// TestE2E_Mv_Rename validates that mv can rename a file within the same folder.
func TestE2E_Mv_Rename(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-mv-rename-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create folder and file.
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/original.txt", "mv rename content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "original.txt", pollTimeout, "ls", "/"+testFolder)

	// Rename.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "mv", "/"+testFolder+"/original.txt", "/"+testFolder+"/renamed.txt")
	assert.Contains(t, stderr, "Moved", "mv should confirm the move")

	// Verify new name exists and old name is gone.
	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "renamed.txt", "renamed file should exist")
	assert.NotContains(t, stdout, "original.txt", "original file should not exist")
}

// Validates: R-1.6
// TestE2E_Mv_MoveToFolder validates that mv can move a file into an existing folder.
func TestE2E_Mv_MoveToFolder(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-mv-tofolder-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create parent folder, subfolder, and file.
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder+"/sub")
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/moveme.txt", "mv to folder content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "moveme.txt", pollTimeout, "ls", "/"+testFolder)

	// Move file into subfolder.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "mv", "/"+testFolder+"/moveme.txt", "/"+testFolder+"/sub")
	assert.Contains(t, stderr, "Moved", "mv should confirm the move")

	// Verify file is now in sub and not in parent.
	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder+"/sub")
	assert.Contains(t, stdout, "moveme.txt", "file should be in subfolder")

	stdout, _ = runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
	assert.NotContains(t, stdout, "moveme.txt", "file should no longer be in parent")
}

// Validates: R-1.6
// TestE2E_Mv_JSON validates that mv --json produces valid JSON output.
func TestE2E_Mv_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-mv-json-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/a.txt", "mv json content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "a.txt", pollTimeout, "ls", "/"+testFolder)

	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "mv", "--json", "/"+testFolder+"/a.txt", "/"+testFolder+"/b.txt")

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result),
		"mv --json should produce valid JSON, got: %s", stdout)
	assert.Contains(t, result, "source", "JSON should have source field")
	assert.Contains(t, result, "destination", "JSON should have destination field")
	assert.Contains(t, result, "id", "JSON should have id field")
}

// Validates: R-1.6
// TestE2E_Mv_NotFound validates that mv with a non-existent source fails.
func TestE2E_Mv_NotFound(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	output := runCLIWithConfigExpectError(t, cfgPath, nil, "mv", "/nonexistent-uuid-mv-12345", "/somewhere")
	assert.Contains(t, output, "nonexistent-uuid-mv-12345", "error should mention the source path")
}

// ---------------------------------------------------------------------------
// cp E2E tests
// ---------------------------------------------------------------------------

// Validates: R-1.7
// TestE2E_Cp_File validates that cp creates a server-side copy of a file.
func TestE2E_Cp_File(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cp-file-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/source.txt", "cp file content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "source.txt", pollTimeout, "ls", "/"+testFolder)

	// Copy to a new name.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "/"+testFolder+"/source.txt", "/"+testFolder+"/copy.txt")
	assert.Contains(t, stderr, "Copied", "cp should confirm the copy")

	// Verify both files exist after the remote copy settles.
	stdout, _ := waitForRemoteReadContains(t, cfgPath, nil, "", "copy.txt", remoteWritePropagationTimeout, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "source.txt", "source file should still exist")
	assert.Contains(t, stdout, "copy.txt", "copied file should exist")

	// Verify content matches.
	content := getRemoteFile(t, cfgPath, nil, "/"+testFolder+"/copy.txt")
	assert.Equal(t, "cp file content", content, "copied file content should match source")
}

// Validates: R-1.7
// TestE2E_Cp_IntoFolder validates that cp into an existing folder works.
func TestE2E_Cp_IntoFolder(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cp-folder-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder+"/dest")
	waitForRemoteReadContains(t, cfgPath, nil, "", "dest", remoteWritePropagationTimeout, "ls", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/src.txt", "cp into folder")
	waitForRemoteReadContains(t, cfgPath, nil, "", "src.txt", pollTimeout, "ls", "/"+testFolder)

	// Copy into the dest folder.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "/"+testFolder+"/src.txt", "/"+testFolder+"/dest")
	assert.Contains(t, stderr, "Copied", "cp should confirm the copy")

	// Verify the copy is in the dest folder.
	stdout, _ := waitForRemoteReadContains(t, cfgPath, nil, "", "src.txt", remoteWritePropagationTimeout, "ls", "/"+testFolder+"/dest")
	assert.Contains(t, stdout, "src.txt", "copied file should appear in dest folder")
}

// Validates: R-1.7
// TestE2E_Cp_JSON validates that cp --json produces valid JSON output.
func TestE2E_Cp_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cp-json-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/j.txt", "cp json")
	waitForRemoteReadContains(t, cfgPath, nil, "", "j.txt", pollTimeout, "ls", "/"+testFolder)

	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "cp", "--json", "/"+testFolder+"/j.txt", "/"+testFolder+"/j-copy.txt")

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result),
		"cp --json should produce valid JSON, got: %s", stdout)
	assert.Contains(t, result, "source", "JSON should have source field")
	assert.Contains(t, result, "destination", "JSON should have destination field")
	assert.Contains(t, result, "id", "JSON should have id field")
}

// Validates: R-1.7
// TestE2E_Cp_NotFound validates that cp with a non-existent source fails.
func TestE2E_Cp_NotFound(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	output := runCLIWithConfigExpectError(t, cfgPath, nil, "cp", "/nonexistent-uuid-cp-12345", "/somewhere")
	assert.Contains(t, output, "nonexistent-uuid-cp-12345", "error should mention the source path")
}

// ---------------------------------------------------------------------------
// mv --force / cp --force E2E tests
// ---------------------------------------------------------------------------

// Validates: R-1.6
// TestE2E_Mv_ForceOverwrite validates that mv without --force fails when the
// destination exists, and mv --force overwrites successfully.
func TestE2E_Mv_ForceOverwrite(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-mv-force-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create folder, source file, and existing destination file.
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/src.txt", "new content")
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/dst.txt", "old content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "src.txt", pollTimeout, "ls", "/"+testFolder)
	waitForRemoteReadContains(t, cfgPath, nil, "", "dst.txt", pollTimeout, "ls", "/"+testFolder)

	// mv without --force should fail (destination already exists).
	output := runCLIWithConfigExpectError(t, cfgPath, nil, "mv", "/"+testFolder+"/src.txt", "/"+testFolder+"/dst.txt")
	assert.Contains(t, output, "already exists", "mv without --force should report destination exists")

	// mv --force should succeed and overwrite.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "mv", "--force", "/"+testFolder+"/src.txt", "/"+testFolder+"/dst.txt")
	assert.Contains(t, stderr, "Moved", "mv --force should confirm the move")

	// Verify destination has source content and source is gone.
	content := getRemoteFile(t, cfgPath, nil, "/"+testFolder+"/dst.txt")
	assert.Equal(t, "new content", content, "destination should have source's content after force move")

	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
	assert.NotContains(t, stdout, "src.txt", "source should no longer exist after move")
	assert.Contains(t, stdout, "dst.txt", "destination should exist")
}

// Validates: R-1.7
// TestE2E_Cp_ForceOverwrite validates that cp without --force fails when the
// destination exists, and cp --force overwrites while preserving the source.
func TestE2E_Cp_ForceOverwrite(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cp-force-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create folder, source file, and existing destination file.
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/src.txt", "copied content")
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/dst.txt", "old content")
	waitForRemoteReadContains(t, cfgPath, nil, "", "src.txt", pollTimeout, "ls", "/"+testFolder)
	waitForRemoteReadContains(t, cfgPath, nil, "", "dst.txt", pollTimeout, "ls", "/"+testFolder)

	// cp without --force should fail (destination already exists).
	output := runCLIWithConfigExpectError(t, cfgPath, nil, "cp", "/"+testFolder+"/src.txt", "/"+testFolder+"/dst.txt")
	assert.Contains(t, output, "already exists", "cp without --force should report destination exists")

	// cp --force should succeed and overwrite.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "--force", "/"+testFolder+"/src.txt", "/"+testFolder+"/dst.txt")
	assert.Contains(t, stderr, "Copied", "cp --force should confirm the copy")

	// Verify destination has source content and source still exists (it's a copy).
	content := getRemoteFile(t, cfgPath, nil, "/"+testFolder+"/dst.txt")
	assert.Equal(t, "copied content", content, "destination should have source's content after force copy")

	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "src.txt", "source should still exist after copy")
	assert.Contains(t, stdout, "dst.txt", "destination should exist")
}

// ---------------------------------------------------------------------------
// mv folder E2E test
// ---------------------------------------------------------------------------

// Validates: R-1.6
// TestE2E_Mv_Folder validates that mv can move a folder with its contents
// to a new location (Graph API PATCH handles folders transparently).
func TestE2E_Mv_Folder(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-mv-folder-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create source directory with a file inside, and a destination parent.
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder+"/source-dir")
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/source-dir/inner.txt", "preserved")
	runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder+"/dest-parent")
	waitForRemoteReadContains(t, cfgPath, nil, "", "source-dir", pollTimeout, "ls", "/"+testFolder)

	// Move folder into dest-parent with a new name.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "mv",
		"/"+testFolder+"/source-dir", "/"+testFolder+"/dest-parent/moved-dir")
	assert.Contains(t, stderr, "Moved", "mv folder should confirm the move")

	// Verify folder contents are at the new location.
	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder+"/dest-parent/moved-dir")
	assert.Contains(t, stdout, "inner.txt", "inner file should be in moved folder")

	// Verify source directory is gone from the parent.
	stdout, _ = runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
	assert.NotContains(t, stdout, "source-dir", "source directory should no longer exist")
	assert.Contains(t, stdout, "dest-parent", "dest-parent should still exist")
}

// ---------------------------------------------------------------------------
// per-drive status condition lifecycle / blocked-delete approval
// ---------------------------------------------------------------------------

// buildDeepPath creates a directory structure under localDir whose relative
// path (from syncRoot) exceeds OneDrive's 400-character path limit. Each
// individual component is a valid OneDrive name, so the scanner processes the
// file normally, but filterInvalidUploads() rejects it with path_too_long.
// Returns (absolute file path, relative path from syncRoot).
func buildDeepPath(t *testing.T, syncDir, testFolder string) (string, string) {
	t.Helper()

	// 200-char directory name + 204-char filename: each passes the scanner's
	// isValidOneDriveName (< 255), but combined with testFolder (~34 chars)
	// the total relative path is ~440 chars > 400.
	longDirName := strings.Repeat("d", 200)
	longFileName := strings.Repeat("f", 200) + ".txt"

	deepDir := filepath.Join(syncDir, testFolder, longDirName)
	deepRelPath := filepath.Join(testFolder, longDirName, longFileName)

	require.Greater(t, len(deepRelPath), 400,
		"relative path must exceed 400 chars to trigger path_too_long failure")

	require.NoError(t, os.MkdirAll(deepDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(deepDir, longFileName), []byte("deep"), 0o600))

	return filepath.Join(deepDir, longFileName), deepRelPath
}

// Validates: R-2.3.3, R-2.3.11
// TestE2E_Status_ConditionLifecycle triggers a real sync condition (path too long),
// fixes the underlying local state, and validates that per-drive status follows
// durable store truth without any manual clear/retry command.
func TestE2E_Status_ConditionLifecycle(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-conditions-readonly-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file whose total relative path exceeds 400 chars.
	_, _ = buildDeepPath(t, syncDir, testFolder)

	// Sync to trigger pre-upload validation failure.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatusSyncState(t, cfgPath, env)
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, "PATH TOO LONG", status.Conditions[0].Title)
	assert.Contains(t, strings.Join(status.Conditions[0].Paths, "\n"), testFolder)

	// Fix the underlying problem by removing the long path and creating
	// a valid replacement that can sync normally.
	longDirName := strings.Repeat("d", 200)
	require.NoError(t, os.RemoveAll(filepath.Join(syncDir, testFolder, longDirName)))
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, testFolder, "fixed.txt"), []byte("fixed"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status = readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, status.Conditions, "status should be clean after the next sync")

	listing, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder)
	assert.Contains(t, listing, "fixed.txt", "replacement file should sync after the condition clears")
}
