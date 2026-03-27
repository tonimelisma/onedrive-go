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
// These tests validate the status, pause, resume, conflicts, resolve, and
// verify CLI commands against a live OneDrive account.
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Check status output.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "Token:", "status should show token state")
	assert.Contains(t, stdout, "ready", "status should show ready state after sync")
}

// TestE2E_Status_JSON validates that status --json produces well-formed JSON
// with the expected schema.
func TestE2E_Status_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Run status --json.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status", "--json")

	var output map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &output),
		"status --json should produce valid JSON, got: %s", stdout)

	// Verify top-level structure.
	assert.Contains(t, output, "accounts", "JSON should have accounts array")
	assert.Contains(t, output, "summary", "JSON should have summary object")

	// Verify summary has expected fields.
	summary, ok := output["summary"].(map[string]interface{})
	require.True(t, ok, "summary should be an object")
	assert.Contains(t, summary, "total_drives", "summary should have total_drives")

	totalDrives, ok := summary["total_drives"].(float64)
	require.True(t, ok, "total_drives should be a number")
	assert.GreaterOrEqual(t, totalDrives, float64(1), "should have at least 1 drive")
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

// TestE2E_Conflicts_EmptyHistory validates that conflicts and conflicts
// --history show appropriate messages when no conflicts exist.
func TestE2E_Conflicts_EmptyHistory(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-noconfl-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state DB.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "clean.txt"), []byte("clean file"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Check conflicts — should show no unresolved.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues")
	assert.Contains(t, stdout, "No issues")

	// Check conflicts --history — should show no history.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "issues", "--history")
	assert.Contains(t, stdout, "No issues in history")
}

// TestE2E_Conflicts_JSON validates that conflicts --json produces a valid
// JSON array with the expected fields.
func TestE2E_Conflicts_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cli-confjson-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "jsonconflict.txt"), []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "jsonconflict.txt"), []byte("local edit"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/jsonconflict.txt", "remote edit")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Check conflicts --json.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues", "--json")

	var issuesJSON map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &issuesJSON),
		"issues --json should produce valid JSON object, got: %s", stdout)

	conflictsRaw, ok := issuesJSON["conflicts"].([]interface{})
	require.True(t, ok, "JSON should have conflicts array")
	require.NotEmpty(t, conflictsRaw, "should have at least one conflict")

	// Verify expected fields.
	conflict, ok := conflictsRaw[0].(map[string]interface{})
	require.True(t, ok, "conflict entry should be an object")
	assert.Contains(t, conflict, "id", "conflict should have id field")
	assert.Contains(t, conflict, "path", "conflict should have path field")
	assert.Contains(t, conflict, "conflict_type", "conflict should have conflict_type field")
	assert.Equal(t, "edit_edit", conflict["conflict_type"], "conflict type should be edit_edit")

	// Resolve to clean up.
	runCLIWithConfig(t, cfgPath, env, "issues", "resolve", "--all", "--keep-remote")
}

// TestE2E_Resolve_KeepBoth validates that resolve --keep-both marks the
// conflict as resolved while preserving both the original and conflict copy.
func TestE2E_Resolve_KeepBoth(t *testing.T) {
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cli-keepboth-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "both.txt"), []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "both.txt"), []byte("local both"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/both.txt", "remote both")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Verify conflict copy exists.
	matches, err := filepath.Glob(filepath.Join(localDir, "both.conflict-*"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "conflict copy should exist before resolve")

	// Resolve --keep-both.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "issues", "resolve", testFolder+"/both.txt", "--keep-both")
	assert.Contains(t, stderr, "Resolved", "resolve should confirm resolution")

	// Verify both files still exist.
	_, err = os.Stat(filepath.Join(localDir, "both.txt"))
	assert.NoError(t, err, "original file should still exist")

	matchesAfter, err := filepath.Glob(filepath.Join(localDir, "both.conflict-*"))
	require.NoError(t, err)
	assert.NotEmpty(t, matchesAfter, "conflict copy should still exist after keep-both")

	// Sync should show no changes (keep-both is fully resolved).
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected",
		"sync after keep-both should show no changes")
}

// TestE2E_Resolve_MultipleStrategies creates 3 conflicts and resolves each
// with a different strategy, then verifies conflict history shows all 3.
func TestE2E_Resolve_MultipleStrategies(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cli-multires-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 3 files and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-original"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-original"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("c-original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create 3 edit-edit conflicts.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("c-local"), 0o600))

	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/a.txt", "a-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/b.txt", "b-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/c.txt", "c-remote")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve each with a different strategy.
	runCLIWithConfig(t, cfgPath, env, "issues", "resolve", testFolder+"/a.txt", "--keep-local")
	runCLIWithConfig(t, cfgPath, env, "issues", "resolve", testFolder+"/b.txt", "--keep-remote")
	runCLIWithConfig(t, cfgPath, env, "issues", "resolve", testFolder+"/c.txt", "--keep-both")

	// Verify conflict history shows all 3.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues", "--history")
	assert.Contains(t, stdout, "a.txt", "history should include a.txt")
	assert.Contains(t, stdout, "b.txt", "history should include b.txt")
	assert.Contains(t, stdout, "c.txt", "history should include c.txt")
	assert.Contains(t, stdout, "keep_local", "history should show keep_local strategy")
	assert.Contains(t, stdout, "keep_remote", "history should show keep_remote strategy")
	assert.Contains(t, stdout, "keep_both", "history should show keep_both strategy")
}

// TestE2E_Resolve_ConflictNotFound validates that resolving a non-existent
// conflict produces an appropriate error.
func TestE2E_Resolve_ConflictNotFound(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Try to resolve a non-existent conflict.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "issues", "resolve", "nonexistent-id", "--keep-local")
	assert.Contains(t, output, "not found", "should report conflict not found")
}

// TestE2E_Verify_AfterSync validates that verify passes after a clean sync
// and produces correct output in both text and JSON formats.
func TestE2E_Verify_AfterSync(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify in text mode.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "verify")
	assert.Contains(t, stdout, "Verified", "verify should report verified files")
	assert.Contains(t, stdout, "All files verified successfully.",
		"verify should confirm all files verified")

	// Verify in JSON mode.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "verify", "--json")

	var verifyOutput map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &verifyOutput),
		"verify --json should produce valid JSON, got: %s", stdout)

	assert.Contains(t, verifyOutput, "verified", "JSON should have verified count")
	assert.Contains(t, verifyOutput, "mismatches", "JSON should have mismatches array")
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
	pollCLIWithConfigContains(t, cfgPath, nil, "original.txt", pollTimeout, "ls", "/"+testFolder)

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
	pollCLIWithConfigContains(t, cfgPath, nil, "moveme.txt", pollTimeout, "ls", "/"+testFolder)

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
	pollCLIWithConfigContains(t, cfgPath, nil, "a.txt", pollTimeout, "ls", "/"+testFolder)

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
	pollCLIWithConfigContains(t, cfgPath, nil, "source.txt", pollTimeout, "ls", "/"+testFolder)

	// Copy to a new name.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "/"+testFolder+"/source.txt", "/"+testFolder+"/copy.txt")
	assert.Contains(t, stderr, "Copied", "cp should confirm the copy")

	// Verify both files exist.
	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder)
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
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/src.txt", "cp into folder")
	pollCLIWithConfigContains(t, cfgPath, nil, "src.txt", pollTimeout, "ls", "/"+testFolder)

	// Copy into the dest folder.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "/"+testFolder+"/src.txt", "/"+testFolder+"/dest")
	assert.Contains(t, stderr, "Copied", "cp should confirm the copy")

	// Verify the copy is in the dest folder.
	stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/"+testFolder+"/dest")
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
	pollCLIWithConfigContains(t, cfgPath, nil, "j.txt", pollTimeout, "ls", "/"+testFolder)

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
// issues clear / issues retry E2E tests
// ---------------------------------------------------------------------------

// Validates: R-2.3.5
// TestE2E_IssuesClear_NoIssues validates that issues clear --all succeeds
// (no-op) when there are no actionable failures.
func TestE2E_IssuesClear_NoIssues(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-clear-none-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state DB.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "ok.txt"), []byte("ok"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Clear all — should succeed with no errors.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues", "clear", "--all")
	assert.Contains(t, stdout, "Cleared all", "clear --all should confirm clearing")
}

// Validates: R-2.3.5
// TestE2E_IssuesClear_NoArg validates that issues clear without arguments
// produces an appropriate error.
func TestE2E_IssuesClear_NoArg(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-clear-noarg-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "ok.txt"), []byte("ok"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// No argument and no --all should fail.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "issues", "clear")
	assert.Contains(t, output, "provide a path", "should guide user to provide argument")
}

// Validates: R-2.3.6
// TestE2E_IssuesRetry_NoIssues validates that issues retry --all succeeds
// (no-op) when there are no failures.
func TestE2E_IssuesRetry_NoIssues(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-retry-none-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "ok.txt"), []byte("ok"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Retry all — should succeed with no errors.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues", "retry", "--all")
	assert.Contains(t, stdout, "Reset all", "retry --all should confirm resetting")
}

// Validates: R-2.3.6
// TestE2E_IssuesRetry_NoArg validates that issues retry without arguments
// produces an appropriate error.
func TestE2E_IssuesRetry_NoArg(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-retry-noarg-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "ok.txt"), []byte("ok"), 0o600))
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// No argument and no --all should fail.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "issues", "retry")
	assert.Contains(t, output, "provide a path", "should guide user to provide argument")
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
	pollCLIWithConfigContains(t, cfgPath, nil, "src.txt", pollTimeout, "ls", "/"+testFolder)
	pollCLIWithConfigContains(t, cfgPath, nil, "dst.txt", pollTimeout, "ls", "/"+testFolder)

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
	pollCLIWithConfigContains(t, cfgPath, nil, "src.txt", pollTimeout, "ls", "/"+testFolder)
	pollCLIWithConfigContains(t, cfgPath, nil, "dst.txt", pollTimeout, "ls", "/"+testFolder)

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
	pollCLIWithConfigContains(t, cfgPath, nil, "source-dir", pollTimeout, "ls", "/"+testFolder)

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
// issues clear / issues retry with actual failures
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

// Validates: R-2.3.5
// TestE2E_IssuesClear_WithFailure triggers a real sync failure (path too long)
// and validates that issues clear <path> removes the specific failure.
func TestE2E_IssuesClear_WithFailure(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-clear-fail-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file whose total relative path exceeds 400 chars.
	_, deepRelPath := buildDeepPath(t, syncDir, testFolder)

	// Sync to trigger pre-upload validation failure.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify the failure was recorded.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues")
	assert.Contains(t, stdout, "PATH TOO LONG", "issues should show path_too_long section")
	assert.Contains(t, stdout, "path exceeds", "issues should describe the path_too_long error")

	// Clear the specific failure by path.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "issues", "clear", deepRelPath)
	assert.Contains(t, stdout, "Cleared failure for", "clear should confirm the action")

	// Verify the failure is gone.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "issues")
	assert.Contains(t, stdout, "No issues", "issues should be clean after clear")
}

// Validates: R-2.3.6
// TestE2E_IssuesRetry_WithFailure validates the full failure retry lifecycle:
// trigger failure → verify → fix the problem → retry → resync → verify clean.
func TestE2E_IssuesRetry_WithFailure(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-retry-fail-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file whose total relative path exceeds 400 chars.
	_, deepRelPath := buildDeepPath(t, syncDir, testFolder)

	// Sync to trigger pre-upload validation failure.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify the failure was recorded.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "issues")
	assert.Contains(t, stdout, "PATH TOO LONG", "issues should show path_too_long section")

	// Fix the problem: remove the deeply nested directory tree.
	longDirName := strings.Repeat("d", 200)
	require.NoError(t, os.RemoveAll(filepath.Join(syncDir, testFolder, longDirName)))

	// Retry the specific failure (resets it for next sync).
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "issues", "retry", deepRelPath)
	assert.Contains(t, stdout, "Reset failure for", "retry should confirm the action")

	// Resync — with the file gone, no new failure should appear.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify clean state.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "issues")
	assert.Contains(t, stdout, "No issues", "issues should be clean after retry and resync")
}
