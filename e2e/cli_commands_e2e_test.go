//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// ---------------------------------------------------------------------------
// CLI command E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests validate the status, pause, resume, resolve, and recover sync
// UX against a live OneDrive account.
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
// summary-plus-per-drive schema.
func TestE2E_Status_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	status := readStatus(t, cfgPath, env)
	assert.Equal(t, 1, status.Summary.TotalDrives)
	driveStatus := requireStatusDrive(t, status, drive)
	require.NotNil(t, driveStatus.SyncState)
	assert.Equal(t, 5, driveStatus.SyncState.ExamplesLimit)
	assert.False(t, driveStatus.SyncState.Verbose)
	assert.NotEmpty(t, driveStatus.SyncState.StateStoreStatus)
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
func TestE2E_Status_PerDrive_NoVisibleProblems(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-cli-noissues-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync to establish state DB.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "clean.txt"), []byte("clean file"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, status.IssueGroups)
	assert.Empty(t, status.DeleteSafety)
	assert.Empty(t, status.Conflicts)
	assert.Empty(t, status.NextActions)
	assert.Equal(t, "healthy", status.StateStoreStatus)
}

// Validates: R-2.3.4
// TestE2E_Status_History_NoConflicts validates that per-drive status and
// status --history show empty conflict sections when no conflicts exist.
func TestE2E_Status_History_NoConflicts(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	current := readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, current.Conflicts)

	history := readStatusSyncState(t, cfgPath, env, "--history")
	assert.Empty(t, history.Conflicts)
	assert.Empty(t, history.ConflictHistory)
}

// Validates: R-2.3.4, R-2.3.10
// TestE2E_Status_JSON_ConflictDetails validates that per-drive status JSON
// exposes unresolved conflicts with the expected fields.
func TestE2E_Status_JSON_ConflictDetails(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/jsonconflict.txt")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "jsonconflict.txt"), []byte("local edit"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/jsonconflict.txt", "remote edit")

	runCLIWithConfig(t, cfgPath, env, "sync")

	status := readStatusSyncState(t, cfgPath, env)
	require.NotEmpty(t, status.Conflicts, "status should report the unresolved conflict")
	conflict := status.Conflicts[0]
	assert.NotEmpty(t, conflict.ID)
	assert.Contains(t, conflict.Path, "jsonconflict.txt")
	assert.Equal(t, "edit_edit", conflict.ConflictType)
	assert.Equal(t, "unresolved", conflict.State)
	assert.Contains(t, conflict.ActionHint, "resolve local")
	assert.NotEmpty(t, status.NextActions)

	// Resolve to clean up.
	queueConflictResolutionAndSync(t, cfgPath, env, "remote", "--all")
}

// Validates: R-2.3.3, R-2.3.4, R-2.3.5
// TestE2E_Resolve_Both_PreservesConflictCopy validates that `resolve both`
// clears the unresolved conflict while preserving both chosen files.
func TestE2E_Resolve_Both_PreservesConflictCopy(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/both.txt")

	// Create edit-edit conflict.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "both.txt"), []byte("local both"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/both.txt", "remote both")

	runCLIWithConfig(t, cfgPath, env, "sync")

	statusBefore := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBefore.Conflicts, 1)
	assert.Contains(t, statusBefore.Conflicts[0].Path, "both.txt")
	assert.Equal(t, "edit_edit", statusBefore.Conflicts[0].ConflictType)
	assert.Empty(t, statusBefore.IssueGroups, "ordinary issues should stay separate from conflicts")

	// Verify conflict copy exists.
	matches, err := filepath.Glob(filepath.Join(localDir, "both.conflict-*"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "conflict copy should exist before resolve")

	// Queue resolve both; the next sync pass owns execution.
	queueConflictResolution(t, cfgPath, env, "both", testFolder+"/both.txt")

	// Verify both files still exist.
	_, err = os.Stat(filepath.Join(localDir, "both.txt"))
	assert.NoError(t, err, "original file should still exist")

	matchesAfter, err := filepath.Glob(filepath.Join(localDir, "both.conflict-*"))
	require.NoError(t, err)
	assert.NotEmpty(t, matchesAfter, "conflict copy should still exist after keep-both")

	// Follow-up sync should leave the owned subtree stable even if unrelated
	// full-suite activity produces delta traffic elsewhere on the shared drive.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")

	statusAfter := readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, statusAfter.Conflicts, "keep-both should clear unresolved conflicts")
}

// Validates: R-2.3.4, R-2.3.5, R-2.3.12
// TestE2E_Status_History_ShowsResolvedStrategies creates 3 conflicts,
// resolves each with a different strategy, and verifies status --history.
func TestE2E_Status_History_ShowsResolvedStrategies(t *testing.T) {
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/a.txt")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/b.txt")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/c.txt")

	// Create 3 edit-edit conflicts.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("c-local"), 0o600))

	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/a.txt", "a-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/b.txt", "b-remote")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/c.txt", "c-remote")

	runCLIWithConfig(t, cfgPath, env, "sync")

	// Resolve each with a different strategy.
	queueConflictResolution(t, cfgPath, env, "local", testFolder+"/a.txt")
	queueConflictResolution(t, cfgPath, env, "remote", testFolder+"/b.txt")
	queueConflictResolution(t, cfgPath, env, "both", testFolder+"/c.txt")
	runCLIWithConfig(t, cfgPath, env, "sync")

	history := readStatusSyncState(t, cfgPath, env, "--history")
	require.Len(t, history.ConflictHistory, 3)
	assert.Contains(t, []string{
		history.ConflictHistory[0].Resolution,
		history.ConflictHistory[1].Resolution,
		history.ConflictHistory[2].Resolution,
	}, "keep_local")
	assert.Contains(t, []string{
		history.ConflictHistory[0].Resolution,
		history.ConflictHistory[1].Resolution,
		history.ConflictHistory[2].Resolution,
	}, "keep_remote")
	assert.Contains(t, []string{
		history.ConflictHistory[0].Resolution,
		history.ConflictHistory[1].Resolution,
		history.ConflictHistory[2].Resolution,
	}, "keep_both")
}

// Validates: R-2.3.5
// TestE2E_Resolve_TargetNotFound validates that resolving a non-existent
// conflict target produces an appropriate error.
func TestE2E_Resolve_TargetNotFound(t *testing.T) {
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

	// Try to resolve a non-existent conflict.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "resolve", "local", "nonexistent-id")
	assert.Contains(t, output, "not found", "should report conflict not found")
}

// Validates: R-2.3.5, R-2.9.3
func TestE2E_Resolve_WithWatchDaemonExecutesQueuedIntent(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-conflicts-watch-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "watch-conflict.txt"), []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, "/"+testFolder+"/watch-conflict.txt")

	require.NoError(t, os.WriteFile(filepath.Join(localDir, "watch-conflict.txt"), []byte("local-watch"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/watch-conflict.txt", "remote-watch")

	runCLIWithConfig(t, cfgPath, env, "sync")

	statusBefore := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBefore.Conflicts, 1)
	assert.Contains(t, statusBefore.Conflicts[0].Path, "watch-conflict.txt")

	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", drive,
		"--debug",
		"sync", "--watch",
	}
	cmd := makeCmd(daemonArgs, env)

	var daemonStdout, daemonStderr syncBuffer
	cmd.Stdout = &daemonStdout
	cmd.Stderr = &daemonStderr

	require.NoError(t, cmd.Start(), "failed to start watch daemon")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, daemonStdout.String(), daemonStderr.String())
	})

	waitForDaemonReady(t, &daemonStderr, 30*time.Second)

	controlStatusBefore := getControlSocketStatus(t, env)
	assert.Equal(t, synccontrol.OwnerModeWatch, controlStatusBefore.OwnerMode)

	queueOutput := queueConflictResolution(t, cfgPath, env, "remote", testFolder+"/watch-conflict.txt")
	assert.Contains(t, queueOutput, "Queued", "watch daemon should accept the queued resolution request")

	require.Eventually(t, func() bool {
		return len(readStatusSyncState(t, cfgPath, env).Conflicts) == 0
	}, 90*time.Second, time.Second, "watch daemon should execute queued conflict resolution")

	require.Eventually(t, func() bool {
		content, err := os.ReadFile(filepath.Join(localDir, "watch-conflict.txt"))
		return err == nil && string(content) == "remote-watch"
	}, 90*time.Second, time.Second, "keep-remote should restore remote content locally")

	require.Eventually(t, func() bool {
		matches, err := filepath.Glob(filepath.Join(localDir, "watch-conflict.conflict-*"))
		return err == nil && len(matches) == 0
	}, 90*time.Second, time.Second, "keep-remote should remove the local conflict copy")

	controlStatusAfter := getControlSocketStatus(t, env)
	assert.Equal(t, 0, controlStatusAfter.PendingConflictRequests)
	assert.Equal(t, 0, controlStatusAfter.ApplyingConflictRequests)
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

	// Verify both files exist after the remote copy settles.
	stdout, _ := pollCLIWithConfigContains(t, cfgPath, nil, "copy.txt", remoteWritePropagationTimeout, "ls", "/"+testFolder)
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
	pollCLIWithConfigContains(t, cfgPath, nil, "dest", remoteWritePropagationTimeout, "ls", "/"+testFolder)
	putRemoteFile(t, cfgPath, nil, "/"+testFolder+"/src.txt", "cp into folder")
	pollCLIWithConfigContains(t, cfgPath, nil, "src.txt", pollTimeout, "ls", "/"+testFolder)

	// Copy into the dest folder.
	_, stderr := runCLIWithConfig(t, cfgPath, nil, "cp", "/"+testFolder+"/src.txt", "/"+testFolder+"/dest")
	assert.Contains(t, stderr, "Copied", "cp should confirm the copy")

	// Verify the copy is in the dest folder.
	stdout, _ := pollCLIWithConfigContains(t, cfgPath, nil, "src.txt", remoteWritePropagationTimeout, "ls", "/"+testFolder+"/dest")
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
// per-drive status issue lifecycle / held-delete approval
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
// TestE2E_Status_IssueLifecycle triggers a real sync failure (path too long),
// fixes the underlying local state, and validates that per-drive status follows
// durable store truth without any manual clear/retry command.
func TestE2E_Status_IssueLifecycle(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-issues-readonly-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file whose total relative path exceeds 400 chars.
	_, _ = buildDeepPath(t, syncDir, testFolder)

	// Sync to trigger pre-upload validation failure.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatusSyncState(t, cfgPath, env)
	require.Len(t, status.IssueGroups, 1)
	assert.Equal(t, "PATH TOO LONG", status.IssueGroups[0].Title)
	assert.Contains(t, strings.Join(status.IssueGroups[0].Paths, "\n"), testFolder)

	// Fix the underlying problem by removing the long path and creating
	// a valid replacement that can sync normally.
	longDirName := strings.Repeat("d", 200)
	require.NoError(t, os.RemoveAll(filepath.Join(syncDir, testFolder, longDirName)))
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, testFolder, "fixed.txt"), []byte("fixed"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status = readStatusSyncState(t, cfgPath, env)
	assert.Empty(t, status.IssueGroups, "status should be clean after the next sync")

	listing, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder)
	assert.Contains(t, listing, "fixed.txt", "replacement file should sync after the issue clears")
}

// Validates: R-2.3.3, R-2.3.6, R-2.3.12, R-6.2.5, R-6.4.2
// TestE2E_Resolve_DeletesWithWatchDaemon validates the watch-mode held-delete
// lifecycle: hold deletes, surface them via status, approve with
// `resolve deletes`, and let watch mode resume delete propagation.
func TestE2E_Resolve_DeletesWithWatchDaemon(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "delete_safety_threshold = 10\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-issues-approve-deletes-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	const fileCount = 12

	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(localDir, name), []byte(fmt.Sprintf("content %d", i)), 0o600))
	}

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	pollCLIWithConfigContains(t, opsCfgPath, nil, "file-12.txt", pollTimeout, "ls", "/"+testFolder)

	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", drive,
		"--debug",
		"sync", "--watch", "--upload-only",
	}
	cmd := makeCmd(daemonArgs, env)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start watch daemon")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, &stderr, 30*time.Second)

	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		require.NoError(t, os.Remove(filepath.Join(localDir, name)))
	}

	require.Eventually(t, func() bool {
		status := readStatusSyncState(t, cfgPath, env, "--verbose")
		return len(status.DeleteSafety) == fileCount
	}, 90*time.Second, time.Second, "status should show held deletes while watch protection is active")

	statusBeforeApproval := readStatusSyncState(t, cfgPath, env, "--verbose")
	require.Len(t, statusBeforeApproval.DeleteSafety, fileCount)

	remoteBeforeApproval, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, remoteBeforeApproval, "file-01.txt", "remote deletes should stay held before approval")

	approvalOutput, _ := runCLIWithConfig(t, cfgPath, env, "resolve", "deletes")
	assert.Contains(t, approvalOutput, "Approved held deletes for this drive.")

	require.Eventually(t, func() bool {
		return len(readStatusSyncState(t, cfgPath, env).DeleteSafety) == 0
	}, 90*time.Second, time.Second, "status should clear delete safety rows once held deletes are approved and processed")

	require.Eventually(t, func() bool {
		remoteListing, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder)
		return !strings.Contains(remoteListing, "file-01.txt")
	}, 90*time.Second, time.Second, "watch daemon should execute approved deletes without an extra manual sync")

	require.Eventually(t, func() bool {
		return getControlSocketStatus(t, env).PendingHeldDeleteApprovals == 0
	}, 90*time.Second, time.Second, "watch daemon should consume approved held-delete rows")

	statusAfterApproval := getControlSocketStatus(t, env)
	assert.Equal(t, synccontrol.OwnerModeWatch, statusAfterApproval.OwnerMode)
	assert.Equal(t, 0, statusAfterApproval.PendingHeldDeleteApprovals)
}
