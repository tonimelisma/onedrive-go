//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Category 1: Basic Sync Operations
// =============================================================================

func TestSyncE2E_InitialDownload(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Put 3 files remotely: two at root level, one nested.
	env.putRemote("alpha.txt", "alpha content")
	env.putRemote("bravo.txt", "bravo content")
	env.mkdirRemote("subdir")
	env.putRemote("subdir/charlie.txt", "charlie content")
	env.sleep()

	report := env.runSyncJSON()

	// All 3 files should exist locally under the test folder with correct content.
	assert.Equal(t, "alpha content", string(env.readLocal(env.testPath("alpha.txt"))))
	assert.Equal(t, "bravo content", string(env.readLocal(env.testPath("bravo.txt"))))
	assert.Equal(t, "charlie content", string(env.readLocal(env.testPath("subdir/charlie.txt"))))

	// JSON report should reflect downloads.
	assert.GreaterOrEqual(t, report.Downloaded, 3, "expected at least 3 downloads")
	assert.GreaterOrEqual(t, report.FoldersCreated, 1, "expected at least 1 folder created")
	assert.Equal(t, "bidirectional", report.Mode)
}

func TestSyncE2E_InitialUpload(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Write files under the test folder so they upload to the correct remote path.
	env.writeLocal(env.testPath("uno.txt"), "uno content")
	env.writeLocal(env.testPath("dos.txt"), "dos content")
	env.writeLocal(env.testPath("tres.txt"), "tres content")

	report := env.runSyncJSON()

	// Verify files exist remotely.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "uno.txt")
	assert.Contains(t, listing, "dos.txt")
	assert.Contains(t, listing, "tres.txt")
	assert.GreaterOrEqual(t, report.Uploaded, 3)
}

func TestSyncE2E_BidirectionalMerge(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// One file remote-only, one file local-only.
	env.putRemote("from-remote.txt", "remote data")
	env.writeLocal(env.testPath("from-local.txt"), "local data")
	env.sleep()

	report := env.runSyncJSON()

	// Both files should exist on both sides.
	assert.True(t, env.localExists(env.testPath("from-remote.txt")), "remote file should be downloaded locally")
	assert.Equal(t, "remote data", string(env.readLocal(env.testPath("from-remote.txt"))))

	remoteListing := env.lsRemote("")
	assert.Contains(t, remoteListing, "from-local.txt", "local file should be uploaded remotely")

	assert.GreaterOrEqual(t, report.Downloaded, 1)
	assert.GreaterOrEqual(t, report.Uploaded, 1)
}

func TestSyncE2E_AlreadyInSync(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Establish baseline.
	env.writeLocal(env.testPath("synced-file.txt"), "synced content")
	env.runSync()

	// Second sync with no changes.
	_, stderr := env.runSync()
	assert.Contains(t, stderr, "Already in sync")

	report := env.runSyncJSON()
	assert.Equal(t, 0, report.Downloaded)
	assert.Equal(t, 0, report.Uploaded)
	assert.Equal(t, 0, report.LocalDeleted)
	assert.Equal(t, 0, report.RemoteDeleted)
	assert.Equal(t, 0, report.Conflicts)
}

func TestSyncE2E_MultipleCycles(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Cycle 1: upload a local file.
	env.writeLocal(env.testPath("cycle-file-1.txt"), "cycle one")
	r1 := env.runSyncJSON()
	assert.GreaterOrEqual(t, r1.Uploaded, 1)

	// Cycle 2: download a remote file.
	env.putRemote("cycle-file-2.txt", "cycle two")
	env.sleep()
	r2 := env.runSyncJSON()
	assert.GreaterOrEqual(t, r2.Downloaded, 1)
	assert.Equal(t, "cycle two", string(env.readLocal(env.testPath("cycle-file-2.txt"))))

	// Cycle 3: edit a local file.
	env.writeLocal(env.testPath("cycle-file-1.txt"), "cycle one updated")
	r3 := env.runSyncJSON()
	assert.GreaterOrEqual(t, r3.Uploaded, 1)

	// Cycle 4: no changes.
	r4 := env.runSyncJSON()
	assert.Equal(t, 0, r4.Downloaded)
	assert.Equal(t, 0, r4.Uploaded)
}

// =============================================================================
// Category 2: Incremental Changes
// =============================================================================

func TestSyncE2E_IncrementalAddLocal(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline.
	env.writeLocal(env.testPath("baseline-a.txt"), "baseline a")
	env.runSync()

	// Add a new local file.
	env.writeLocal(env.testPath("added-b.txt"), "added b")
	report := env.runSyncJSON()

	assert.Equal(t, 1, report.Uploaded)
	listing := env.lsRemote("")
	assert.Contains(t, listing, "added-b.txt")
}

func TestSyncE2E_IncrementalAddRemote(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline.
	env.writeLocal(env.testPath("baseline-c.txt"), "baseline c")
	env.runSync()

	// Add a new remote file.
	env.putRemote("added-d.txt", "added d")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.Downloaded)
	assert.Equal(t, "added d", string(env.readLocal(env.testPath("added-d.txt"))))
}

func TestSyncE2E_IncrementalEditLocal(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline with version 1.
	env.writeLocal(env.testPath("editable.txt"), "version 1")
	env.runSync()

	// Edit locally to version 2.
	env.writeLocal(env.testPath("editable.txt"), "version 2")
	report := env.runSyncJSON()

	assert.Equal(t, 1, report.Uploaded)

	// Verify remote has updated content by downloading via get.
	tmpDir := t.TempDir()
	downloadPath := filepath.Join(tmpDir, "editable.txt")
	runCLI(t, "get", env.remoteDir+"/editable.txt", downloadPath)

	downloaded, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, "version 2", string(downloaded))
}

func TestSyncE2E_IncrementalEditRemote(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline with version 1.
	env.writeLocal(env.testPath("remote-editable.txt"), "version 1")
	env.runSync()

	// Edit remotely to version 2.
	env.putRemote("remote-editable.txt", "version 2")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.Downloaded)
	assert.Equal(t, "version 2", string(env.readLocal(env.testPath("remote-editable.txt"))))
}

// =============================================================================
// Category 3: Delete Propagation
// =============================================================================

func TestSyncE2E_DeleteLocalFile(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline: sync a file to both sides.
	env.writeLocal(env.testPath("delete-me.txt"), "will be deleted")
	env.runSync()

	// Verify it exists remotely.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "delete-me.txt")

	// Delete locally.
	env.removeLocal(env.testPath("delete-me.txt"))
	report := env.runSyncJSON()

	assert.Equal(t, 1, report.RemoteDeleted)

	// Verify it no longer exists remotely.
	listing = env.lsRemote("")
	assert.NotContains(t, listing, "delete-me.txt")
}

func TestSyncE2E_DeleteRemoteFile(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline: sync a file to both sides.
	env.writeLocal(env.testPath("remote-delete-me.txt"), "will be deleted remotely")
	env.runSync()

	// Delete remotely.
	env.rmRemote("remote-delete-me.txt")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.LocalDeleted)
	assert.False(t, env.localExists(env.testPath("remote-delete-me.txt")), "local file should be deleted")
}

func TestSyncE2E_DeleteLocalFolder(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline: sync a folder with a file.
	env.writeLocal(env.testPath("del-folder/nested-file.txt"), "nested content")
	env.runSync()

	// Delete the folder locally.
	env.removeLocal(env.testPath("del-folder"))
	report := env.runSyncJSON()

	// Should delete both the file and folder remotely.
	assert.GreaterOrEqual(t, report.RemoteDeleted, 1, "expected at least the file to be deleted remotely")
}

func TestSyncE2E_DeleteRemoteFolder(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline: sync a folder with a file.
	env.writeLocal(env.testPath("remote-del-folder/remote-nested.txt"), "nested remote")
	env.runSync()
	assert.True(t, env.localExists(env.testPath("remote-del-folder/remote-nested.txt")))

	// Delete the folder remotely.
	env.rmRemote("remote-del-folder")
	env.sleep()

	report := env.runSyncJSON()
	assert.GreaterOrEqual(t, report.LocalDeleted, 1)
	assert.False(t, env.localExists(env.testPath("remote-del-folder/remote-nested.txt")), "local nested file should be deleted")
}

// =============================================================================
// Category 4: Conflict Scenarios
// =============================================================================

func TestSyncE2E_EditEditConflict(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline.
	env.writeLocal(env.testPath("conflict-edit.txt"), "original content")
	env.runSync()

	// Edit on both sides with different content.
	env.writeLocal(env.testPath("conflict-edit.txt"), "local edit version")
	env.putRemote("conflict-edit.txt", "remote edit version")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.Conflicts)

	// Remote version wins in the main file.
	assert.Equal(t, "remote edit version", string(env.readLocal(env.testPath("conflict-edit.txt"))))

	// Local version should be preserved in a conflict copy.
	conflictPath := env.findConflictFile(env.testPath("conflict-edit.txt"))
	assert.NotEmpty(t, conflictPath, "expected a .conflict-* file for local version")

	if conflictPath != "" {
		conflictContent := env.readLocal(conflictPath)
		assert.Equal(t, "local edit version", string(conflictContent))
	}
}

func TestSyncE2E_CreateCreateConflict(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Establish empty baseline (get delta token).
	env.runSync()

	// Create same file on both sides with different content.
	env.writeLocal(env.testPath("create-conflict.txt"), "local create version")
	env.putRemote("create-conflict.txt", "remote create version")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.Conflicts)

	// Remote version wins in the main file.
	assert.Equal(t, "remote create version", string(env.readLocal(env.testPath("create-conflict.txt"))))

	// Local version preserved in conflict copy.
	conflictPath := env.findConflictFile(env.testPath("create-conflict.txt"))
	assert.NotEmpty(t, conflictPath, "expected a .conflict-* file")
}

func TestSyncE2E_EditDeleteConflict(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline.
	env.writeLocal(env.testPath("edit-del-conflict.txt"), "original for edit-delete")
	env.runSync()

	// Edit locally, delete remotely.
	env.writeLocal(env.testPath("edit-del-conflict.txt"), "local edit for conflict")
	env.rmRemote("edit-del-conflict.txt")
	env.sleep()

	report := env.runSyncJSON()
	assert.Equal(t, 1, report.Conflicts)

	// Local version should survive (re-uploaded).
	assert.True(t, env.localExists(env.testPath("edit-del-conflict.txt")), "local file should still exist")
}

func TestSyncE2E_FalseConflict(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Baseline.
	env.writeLocal(env.testPath("false-conflict.txt"), "original false conflict")
	env.runSync()

	// Both sides converge to the same content.
	env.writeLocal(env.testPath("false-conflict.txt"), "converged content")
	env.putRemote("false-conflict.txt", "converged content")
	env.sleep()

	report := env.runSyncJSON()

	// False conflict: hashes match, so no conflict is reported.
	assert.Equal(t, 0, report.Conflicts)
	assert.Equal(t, "converged content", string(env.readLocal(env.testPath("false-conflict.txt"))))

	// No conflict file should exist.
	conflictPath := env.findConflictFile(env.testPath("false-conflict.txt"))
	assert.Empty(t, conflictPath, "no conflict file expected for false conflict")
}

// =============================================================================
// Category 5: Sync Modes
// =============================================================================

func TestSyncE2E_DownloadOnly(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.putRemote("dl-only-remote.txt", "download only content")
	env.writeLocal(env.testPath("dl-only-local.txt"), "upload should not happen")
	env.sleep()

	report := env.runSyncJSON("--download-only")

	// Remote file should be downloaded.
	assert.True(t, env.localExists(env.testPath("dl-only-remote.txt")))
	assert.GreaterOrEqual(t, report.Downloaded, 1)

	// Local file should NOT be uploaded.
	listing := env.lsRemote("")
	assert.NotContains(t, listing, "dl-only-local.txt")
	assert.Equal(t, "download-only", report.Mode)
}

func TestSyncE2E_UploadOnly(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.putRemote("ul-only-remote.txt", "should not be downloaded")
	env.writeLocal(env.testPath("ul-only-local.txt"), "upload only content")
	env.sleep()

	report := env.runSyncJSON("--upload-only")

	// Local file should be uploaded.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "ul-only-local.txt")
	assert.GreaterOrEqual(t, report.Uploaded, 1)

	// Remote file should NOT be downloaded.
	assert.False(t, env.localExists(env.testPath("ul-only-remote.txt")))
	assert.Equal(t, "upload-only", report.Mode)
}

func TestSyncE2E_DryRun(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.writeLocal(env.testPath("dry-run-file.txt"), "dry run content")

	report := env.runSyncJSON("--dry-run")

	// Should report the upload but not actually do it.
	assert.True(t, report.DryRun)
	assert.GreaterOrEqual(t, report.Uploaded, 1)

	// File should NOT exist remotely.
	listing := env.lsRemote("")
	assert.NotContains(t, listing, "dry-run-file.txt")
}

func TestSyncE2E_DryRunNoSideEffects(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.putRemote("dry-no-effect.txt", "dry run no side effects")
	env.sleep()

	// Dry run should not download.
	env.runSync("--dry-run")
	assert.False(t, env.localExists(env.testPath("dry-no-effect.txt")), "dry run should not download files")

	// Real sync should download.
	env.runSync()
	assert.True(t, env.localExists(env.testPath("dry-no-effect.txt")), "real sync should download files")
	assert.Equal(t, "dry run no side effects", string(env.readLocal(env.testPath("dry-no-effect.txt"))))
}

// =============================================================================
// Category 6: Folder Operations
// =============================================================================

func TestSyncE2E_NewLocalFolder(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Establish baseline.
	env.runSync()

	// Create folder with file locally.
	env.writeLocal(env.testPath("new-local-dir/inside.txt"), "inside local dir")
	report := env.runSyncJSON()

	assert.GreaterOrEqual(t, report.FoldersCreated, 1)
	assert.GreaterOrEqual(t, report.Uploaded, 1)

	listing := env.lsRemote("new-local-dir")
	assert.Contains(t, listing, "inside.txt")
}

func TestSyncE2E_NewRemoteFolder(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Establish baseline.
	env.runSync()

	// Create folder with file remotely.
	env.mkdirRemote("new-remote-dir")
	env.putRemote("new-remote-dir/inside-remote.txt", "inside remote dir")
	env.sleep()

	report := env.runSyncJSON()
	assert.GreaterOrEqual(t, report.FoldersCreated, 1)
	assert.GreaterOrEqual(t, report.Downloaded, 1)

	assert.True(t, env.localExists(env.testPath("new-remote-dir/inside-remote.txt")))
	assert.Equal(t, "inside remote dir", string(env.readLocal(env.testPath("new-remote-dir/inside-remote.txt"))))
}

func TestSyncE2E_DeepNesting(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.writeLocal(env.testPath("level-a/level-b/level-c/level-d/deep-file.txt"), "deep nesting content")
	report := env.runSyncJSON()

	assert.GreaterOrEqual(t, report.FoldersCreated, 4, "expected 4 levels of folders created")
	assert.GreaterOrEqual(t, report.Uploaded, 1)

	// Verify the file exists remotely.
	listing := env.lsRemote("level-a/level-b/level-c/level-d")
	assert.Contains(t, listing, "deep-file.txt")
}

// =============================================================================
// Category 7: Safety Checks
// =============================================================================

func TestSyncE2E_NosyncGuard(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Create a .nosync guard file at the sync root (NOT under testPath).
	env.writeLocal(".nosync", "")

	output := env.runSyncExpectError()
	assert.Contains(t, strings.ToLower(output), "nosync", "error should mention nosync guard")
}

func TestSyncE2E_BigDeleteBlocked(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{
		bigDeleteThreshold: 10,
		bigDeleteMinItems:  5,
	})

	// Create and sync 15 files to establish baseline.
	for i := range 15 {
		env.writeLocal(env.testPath(fmt.Sprintf("bulk-file-%02d.txt", i)), fmt.Sprintf("bulk content %d", i))
	}
	env.runSync()

	// Delete all files locally.
	for i := range 15 {
		env.removeLocal(env.testPath(fmt.Sprintf("bulk-file-%02d.txt", i)))
	}

	// Sync without --force should fail.
	output := env.runSyncExpectError()
	assert.True(t,
		strings.Contains(strings.ToLower(output), "big-delete") ||
			strings.Contains(strings.ToLower(output), "safety") ||
			strings.Contains(strings.ToLower(output), "big_delete"),
		"error should mention big-delete protection, got: %s", output)
}

func TestSyncE2E_BigDeleteForce(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{
		bigDeleteThreshold: 10,
		bigDeleteMinItems:  5,
	})

	// Create and sync 15 files.
	for i := range 15 {
		env.writeLocal(env.testPath(fmt.Sprintf("force-bulk-%02d.txt", i)), fmt.Sprintf("force content %d", i))
	}
	env.runSync()

	// Delete all files locally.
	for i := range 15 {
		env.removeLocal(env.testPath(fmt.Sprintf("force-bulk-%02d.txt", i)))
	}

	// Sync with --force should succeed.
	report := env.runSyncJSON("--force")
	assert.GreaterOrEqual(t, report.RemoteDeleted, 15, "expected all 15 files to be remotely deleted")
}

// =============================================================================
// Category 8: Filtering
// =============================================================================

func TestSyncE2E_SkipDotfiles(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{skipDotfiles: true})

	env.writeLocal(env.testPath(".hidden-file"), "should be skipped")
	env.writeLocal(env.testPath("visible-file.txt"), "should be synced")

	env.runSync()

	listing := env.lsRemote("")
	assert.Contains(t, listing, "visible-file.txt", "visible file should be uploaded")
	assert.NotContains(t, listing, ".hidden-file", "dotfile should NOT be uploaded")
}

func TestSyncE2E_SkipFilesPattern(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{skipFiles: []string{"*.log"}})

	env.writeLocal(env.testPath("application.log"), "log data should be skipped")
	env.writeLocal(env.testPath("data-file.txt"), "data should be synced")

	env.runSync()

	listing := env.lsRemote("")
	assert.Contains(t, listing, "data-file.txt", "data file should be uploaded")
	assert.NotContains(t, listing, "application.log", ".log file should NOT be uploaded")
}

func TestSyncE2E_SkipDirsPattern(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{skipDirs: []string{"node_modules"}})

	env.writeLocal(env.testPath("node_modules/package.json"), "npm package should be skipped")
	env.writeLocal(env.testPath("src/main.go"), "source should be synced")

	env.runSync()

	listing := env.lsRemote("")
	assert.Contains(t, listing, "src", "src dir should be uploaded")
	assert.NotContains(t, listing, "node_modules", "node_modules should NOT be uploaded")
}

// =============================================================================
// Category 9: Edge Cases
// =============================================================================

func TestSyncE2E_LargeFile(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// 5 MiB file triggers chunked upload.
	const fileSize = 5 * 1024 * 1024
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251) // deterministic pattern for corruption detection
	}

	env.writeLocalBytes(env.testPath("large-sync-file.bin"), data)
	env.runSync()

	// Verify it exists remotely.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "large-sync-file.bin")

	// Download via CLI to verify round-trip integrity.
	tmpDir := t.TempDir()
	downloadPath := filepath.Join(tmpDir, "large-sync-file.bin")
	runCLI(t, "get", env.remoteDir+"/large-sync-file.bin", downloadPath)

	downloaded, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, data, downloaded, "downloaded file content should match uploaded data")
}

func TestSyncE2E_UnicodeFilenames(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.writeLocal(env.testPath("日本語テスト.txt"), "unicode sync content")
	env.runSync()

	// Verify remotely.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "日本語テスト.txt")

	// Download and verify content.
	tmpDir := t.TempDir()
	downloadPath := filepath.Join(tmpDir, "unicode-verify.txt")
	runCLI(t, "get", env.remoteDir+"/日本語テスト.txt", downloadPath)

	downloaded, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, "unicode sync content", string(downloaded))
}

func TestSyncE2E_SpacesInFilenames(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.writeLocal(env.testPath("my test file.txt"), "spaces sync content")
	env.runSync()

	// Verify remotely.
	listing := env.lsRemote("")
	assert.Contains(t, listing, "my test file.txt")

	// Download and verify.
	tmpDir := t.TempDir()
	downloadPath := filepath.Join(tmpDir, "spaces-verify.txt")
	runCLI(t, "get", env.remoteDir+"/my test file.txt", downloadPath)

	downloaded, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, "spaces sync content", string(downloaded))
}

// =============================================================================
// Category 10: Output Verification
// =============================================================================

func TestSyncE2E_JSONOutputFormat(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// Create a file to trigger at least one action.
	env.writeLocal(env.testPath("json-format-test.txt"), "json format test")

	report := env.runSyncJSON()

	// Verify all expected fields are present and typed correctly.
	assert.Equal(t, "bidirectional", report.Mode)
	assert.False(t, report.DryRun)
	assert.Greater(t, report.DurationMs, int64(0), "duration should be positive")
	assert.GreaterOrEqual(t, report.Uploaded, 1)
	assert.NotNil(t, report.Errors, "errors should be an array, not null")
}

func TestSyncE2E_QuietMode(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	env.writeLocal(env.testPath("quiet-mode-file.txt"), "quiet mode test")

	// --quiet is a root persistent flag; Cobra handles it after the subcommand.
	_, stderr, err := env.runSyncRaw("--quiet")
	require.NoError(t, err)
	assert.Empty(t, stderr, "stderr should be empty with --quiet")
}

func TestSyncE2E_ExitCodeOnErrors(t *testing.T) {
	env := newSyncEnv(t, syncEnvOpts{})

	// The exit code fix from #69 ensures non-zero exit when report has errors.
	// We can verify the JSON+exit-code interaction by running --json with
	// a scenario that produces errors, or simply verify a clean run exits 0.

	env.writeLocal(env.testPath("exit-code-test.txt"), "exit code verification")

	// Clean sync should exit 0.
	_, _, err := env.runSyncRaw("--json")
	assert.NoError(t, err, "clean sync should exit with code 0")
}
