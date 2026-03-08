//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "status-test.txt"), []byte("status test"), 0o644))

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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "clean.txt"), []byte("clean file"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Check conflicts — should show no unresolved.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "conflicts")
	assert.Contains(t, stdout, "No unresolved conflicts")

	// Check conflicts --history — should show no history.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "conflicts", "--history")
	assert.Contains(t, stdout, "No conflicts in history")
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "jsonconflict.txt"), []byte("original"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "jsonconflict.txt"), []byte("local edit"), 0o644))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/jsonconflict.txt", "remote edit")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Check conflicts --json.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "conflicts", "--json")

	var conflicts []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &conflicts),
		"conflicts --json should produce valid JSON array, got: %s", stdout)

	require.NotEmpty(t, conflicts, "should have at least one conflict")

	// Verify expected fields.
	conflict := conflicts[0]
	assert.Contains(t, conflict, "id", "conflict should have id field")
	assert.Contains(t, conflict, "path", "conflict should have path field")
	assert.Contains(t, conflict, "conflict_type", "conflict should have conflict_type field")
	assert.Equal(t, "edit_edit", conflict["conflict_type"], "conflict type should be edit_edit")

	// Resolve to clean up.
	runCLIWithConfig(t, cfgPath, env, "resolve", "--all", "--keep-remote")
}

// TestE2E_Resolve_KeepBoth validates that resolve --keep-both marks the
// conflict as resolved while preserving both the original and conflict copy.
func TestE2E_Resolve_KeepBoth(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-cli-keepboth-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "both.txt"), []byte("original"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "both.txt"), []byte("local both"), 0o644))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/both.txt", "remote both")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Verify conflict copy exists.
	matches, err := filepath.Glob(filepath.Join(localDir, "both.conflict-*"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "conflict copy should exist before resolve")

	// Resolve --keep-both.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/both.txt", "--keep-both")
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-original"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-original"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("c-original"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Create 3 edit-edit conflicts.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-local"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-local"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("c-local"), 0o644))

	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/a.txt", "a-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/b.txt", "b-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/c.txt", "c-remote")

	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve each with a different strategy.
	runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/a.txt", "--keep-local")
	runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/b.txt", "--keep-remote")
	runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/c.txt", "--keep-both")

	// Verify conflict history shows all 3.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "conflicts", "--history")
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "dummy.txt"), []byte("dummy"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Try to resolve a non-existent conflict.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "resolve", "nonexistent-id", "--keep-local")
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v1.txt"), []byte("verify 1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v2.txt"), []byte("verify 2"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "v3.txt"), []byte("verify 3"), 0o644))

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
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-recycle-bin-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Upload a file.
	localFile := filepath.Join(syncDir, "recycle-test.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("recycle bin test content"), 0o644))
	runCLIWithConfig(t, cfgPath, env, "put", localFile, testFolder+"/recycle-test.txt")

	// Get the item ID via ls --json.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "ls", testFolder, "--json")
	var lsItems []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &lsItems),
		"ls --json should produce valid JSON, got: %s", stdout)
	require.Len(t, lsItems, 1)
	itemID, ok := lsItems[0]["id"].(string)
	require.True(t, ok, "item should have an id field")

	// Delete the file (moves to recycle bin).
	runCLIWithConfig(t, cfgPath, env, "rm", testFolder+"/recycle-test.txt")

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
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "ls", testFolder, "--json")
	require.NoError(t, json.Unmarshal([]byte(stdout), &lsItems),
		"ls --json after restore should produce valid JSON, got: %s", stdout)
	require.Len(t, lsItems, 1)
	assert.Equal(t, "recycle-test.txt", lsItems[0]["name"])
}

// TestE2E_RecycleBinEmpty validates that recycle-bin empty --confirm
// permanently removes items from the recycle bin.
func TestE2E_RecycleBinEmpty(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-recycle-empty-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Upload and delete a file to put it in the recycle bin.
	localFile := filepath.Join(syncDir, "empty-test.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("will be permanently deleted"), 0o644))
	runCLIWithConfig(t, cfgPath, env, "put", localFile, testFolder+"/empty-test.txt")
	runCLIWithConfig(t, cfgPath, env, "rm", testFolder+"/empty-test.txt")

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
