//go:build e2e && e2e_full

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Comprehensive sync E2E tests (slow — run only with -tags=e2e,e2e_full)
// ---------------------------------------------------------------------------

// TestE2E_EdgeCases covers edge cases: large files (resumable upload),
// unicode filenames, spaces in filenames, and concurrent uploads.
func TestE2E_EdgeCases(t *testing.T) {
	registerLogDump(t)
	opsCfgPath := writeMinimalConfig(t)
	testFolderPrefix := fmt.Sprintf("onedrive-go-e2e-edge-%d", time.Now().UnixNano())

	t.Run("large_file_upload_download", func(t *testing.T) {
		testLargeFileUploadDownload(t, opsCfgPath, makeEdgeCaseTestFolder(t, opsCfgPath, testFolderPrefix, "large"))
	})

	t.Run("unicode_filename", func(t *testing.T) {
		testUnicodeFilename(t, opsCfgPath, makeEdgeCaseTestFolder(t, opsCfgPath, testFolderPrefix, "unicode"))
	})

	t.Run("spaces_in_filename", func(t *testing.T) {
		testSpacesInFilename(t, opsCfgPath, makeEdgeCaseTestFolder(t, opsCfgPath, testFolderPrefix, "spaces"))
	})

	t.Run("concurrent_uploads", func(t *testing.T) {
		testConcurrentUploads(t, opsCfgPath, makeEdgeCaseTestFolder(t, opsCfgPath, testFolderPrefix, "concurrent"))
	})
}

func makeEdgeCaseTestFolder(t *testing.T, opsCfgPath, prefix, suffix string) string {
	t.Helper()

	testFolder := fmt.Sprintf("%s-%s", prefix, suffix)
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	return testFolder
}

// testLargeFileUploadDownload generates a 5 MiB file (exceeding the 4 MB
// simple-upload threshold) to exercise the resumable upload path.
func testLargeFileUploadDownload(t *testing.T, opsCfgPath, testFolder string) {
	t.Helper()

	const fileSize = 5 * 1024 * 1024 // 5 MiB — triggers CreateUploadSession

	// Generate deterministic data using a prime modulus so every byte
	// position has a distinct-enough value for corruption detection.
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251)
	}

	tmpFile, err := os.CreateTemp("", "e2e-large-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(data)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	remotePath := "/" + testFolder + "/large-file.bin"

	// Upload — should trigger resumable upload (>4 MB).
	_, stderr := runCLIWithConfig(t, opsCfgPath, nil, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// Poll for eventual consistency — file may not be visible immediately.
	pollCLIWithConfigContains(t, opsCfgPath, nil, fmt.Sprintf("%d bytes", fileSize), pollTimeout, "stat", remotePath)

	// Download and verify byte-for-byte content integrity.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "large-file.bin")

	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, data, downloaded, "downloaded file content does not match uploaded data")

	// Cleanup the remote file.
	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testUnicodeFilename verifies that files with non-ASCII (Japanese) characters
// in the filename can be uploaded, listed, downloaded, and deleted.
func testUnicodeFilename(t *testing.T, opsCfgPath, testFolder string) {
	t.Helper()

	content := []byte("Unicode test content\n")
	remoteName := "日本語テスト.txt"
	remotePath := "/" + testFolder + "/" + remoteName

	tmpFile, err := os.CreateTemp("", "e2e-unicode-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	// Upload with unicode filename.
	_, stderr := runCLIWithConfig(t, opsCfgPath, nil, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// Poll for eventual consistency — file may not be visible immediately.
	pollCLIWithConfigContains(t, opsCfgPath, nil, remoteName, pollTimeout, "ls", "/"+testFolder)

	// Download and verify content.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "downloaded-unicode.txt")

	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)

	// Cleanup.
	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testSpacesInFilename verifies that filenames containing spaces are handled
// correctly through upload, stat, download, and delete.
func testSpacesInFilename(t *testing.T, opsCfgPath, testFolder string) {
	t.Helper()

	content := []byte("Spaces test content\n")
	remoteName := "my test file.txt"
	remotePath := "/" + testFolder + "/" + remoteName

	tmpFile, err := os.CreateTemp("", "e2e-spaces-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	// Upload with spaces in filename.
	_, stderr := runCLIWithConfig(t, opsCfgPath, nil, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// Poll for eventual consistency — file may not be visible immediately.
	pollCLIWithConfigContains(t, opsCfgPath, nil, remoteName, pollTimeout, "stat", remotePath)

	// Download and verify content.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "downloaded-spaces.txt")

	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)

	// Cleanup.
	_, stderr = runCLIWithConfig(t, opsCfgPath, nil, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testFile holds local and remote paths for a file used in concurrent upload tests.
type testFile struct {
	localPath  string
	remoteName string
}

// testConcurrentUploads verifies that multiple files can be uploaded in
// parallel without errors or data corruption.
func testConcurrentUploads(t *testing.T, opsCfgPath, testFolder string) {
	t.Helper()

	const fileCount = 3

	files := make([]testFile, fileCount)
	for i := range files {
		content := []byte(fmt.Sprintf("concurrent file %d content\n", i))

		tmpFile, err := os.CreateTemp("", fmt.Sprintf("e2e-concurrent-%d-*", i))
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write(content)
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		files[i] = testFile{
			localPath:  tmpFile.Name(),
			remoteName: fmt.Sprintf("concurrent-%d.txt", i),
		}
	}

	// Upload all files in parallel. We use makeCmd directly instead
	// of runCLIWithConfig because t.Fatalf panics when called from non-test goroutines.
	errCh := make(chan error, fileCount)

	for i := range files {
		go func(f testFile) {
			remotePath := "/" + testFolder + "/" + f.remoteName
			fullArgs := []string{"--drive", drive, "put", f.localPath, remotePath}
			cmd := makeCmd(fullArgs, nil)

			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			if runErr := cmd.Run(); runErr != nil {
				errCh <- fmt.Errorf("uploading %s: %v\nstderr: %s",
					f.remoteName, runErr, stderr.String())
				return
			}

			errCh <- nil
		}(files[i])
	}

	for range fileCount {
		err := <-errCh
		require.NoError(t, err)
	}

	// Poll for eventual consistency — files may not all be visible immediately.
	// Wait until the last uploaded file appears in the listing.
	stdout, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, files[len(files)-1].remoteName, pollTimeout, "ls", "/"+testFolder)
	for _, f := range files {
		assert.Contains(t, stdout, f.remoteName,
			"expected %s in folder listing", f.remoteName)
	}

	// Cleanup all uploaded files.
	for _, f := range files {
		remotePath := "/" + testFolder + "/" + f.remoteName
		_, stderr := runCLIWithConfig(t, opsCfgPath, nil, "rm", remotePath)
		assert.Contains(t, stderr, "Deleted")
	}
}

// TestE2E_Sync_BidirectionalMerge exercises bidirectional merge:
// EF1 (unchanged), EF3 (local edit→upload), EF13 (new local→upload),
// EF14 (new remote→download), ED3 (new remote folder), ED5 (new local folder),
// verify, and idempotent re-sync.
func TestE2E_Sync_BidirectionalMerge(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-bidi-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local files in a docs/ subfolder.
	docsDir := filepath.Join(syncDir, testFolder, "docs")
	require.NoError(t, os.MkdirAll(docsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "readme.txt"), []byte("readme content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "notes.txt"), []byte("notes content"), 0o600))

	// Step 2: Upload-only sync to establish baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 3: Assert files exist remotely (poll for eventual consistency).
	stdout, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, "readme.txt", pollTimeout, "ls", "/"+testFolder+"/docs")
	assert.Contains(t, stdout, "notes.txt")

	// Step 4: Create new local folder + file (EF13 + ED5).
	localOnlyDir := filepath.Join(syncDir, testFolder, "local-only")
	require.NoError(t, os.MkdirAll(localOnlyDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localOnlyDir, "stuff.txt"), []byte("local stuff"), 0o600))

	// Step 5: Create new remote folder + file (EF14 + ED3).
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder+"/data")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/data/info.txt", "remote info data")

	// Step 6: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 7: Assert merge results.
	// stuff.txt uploaded remotely (EF13 + ED5).
	stdout, _ = runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder+"/local-only")
	assert.Contains(t, stdout, "stuff.txt")

	// info.txt downloaded locally (EF14 + ED3).
	infoData, err := os.ReadFile(filepath.Join(syncDir, testFolder, "data", "info.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote info data", string(infoData))

	// readme.txt unchanged (EF1).
	readmeData, err := os.ReadFile(filepath.Join(syncDir, testFolder, "docs", "readme.txt"))
	require.NoError(t, err)
	assert.Equal(t, "readme content", string(readmeData))

	// Step 8: Internal baseline verification should report a clean tree.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)

	// Step 9: Re-sync should leave the test-owned subtree unchanged even if
	// unrelated live-drive activity produces delta events elsewhere.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, filepath.Join(syncDir, testFolder), "sync")
}

// TestE2E_Sync_EditEditConflict_ResolveKeepRemote exercises EF5 (edit-edit
// conflict), conflict copy creation, and resolve remote.
func TestE2E_Sync_EditEditConflict_ResolveKeepRemote(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-ee-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	conflictFile := filepath.Join(localDir, "conflict-file.txt")
	require.NoError(t, os.WriteFile(conflictFile, []byte("original v1"), 0o600))

	// Step 2: Upload-only sync.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 3: Modify local.
	require.NoError(t, os.WriteFile(conflictFile, []byte("local edit v2"), 0o600))

	// Step 4: Modify remote with different content.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/conflict-file.txt", "remote edit v2")

	// Step 5: Bidirectional sync — should detect conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 6: Conflict reported in sync output.
	assert.Contains(t, stderr, "Conflicts:")

	// Step 7: Per-drive status should report the unresolved conflict.
	statusBeforeResolve := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBeforeResolve.Conflicts, 1)
	assert.Contains(t, statusBeforeResolve.Conflicts[0].Path, "conflict-file.txt")
	assert.Equal(t, "edit_edit", statusBeforeResolve.Conflicts[0].ConflictType)

	// Step 8: Conflict copy exists on disk.
	matches, err := filepath.Glob(filepath.Join(localDir, "conflict-file.conflict-*.txt"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "expected exactly 1 conflict copy")

	// Step 9: Conflict copy contains local content.
	conflictCopyData, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, "local edit v2", string(conflictCopyData))

	// Step 10: Original path has remote content (downloaded).
	originalData, err := os.ReadFile(conflictFile)
	require.NoError(t, err)
	assert.Equal(t, "remote edit v2", string(originalData))

	// Step 11: Queue resolve remote; a normal sync executes the request.
	queueConflictResolutionAndSync(t, cfgPath, env, "remote", testFolder+"/conflict-file.txt")

	// Step 13: No more conflicts.
	assert.Empty(t, readStatusSyncState(t, cfgPath, env).Conflicts)

	// Step 14: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_EditDeleteConflict exercises EF9 (edit-delete conflict)
// auto-resolve: local edit wins. The modified local file is uploaded to
// re-create the remote, and a resolved conflict is recorded in history.
func TestE2E_Sync_EditDeleteConflict(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-ed-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	fragileFile := filepath.Join(localDir, "fragile.txt")
	require.NoError(t, os.WriteFile(fragileFile, []byte("precious data"), 0o600))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Advance delta token past the upload so the subsequent deletion is
	// visible via incremental delta (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Step 3: Modify local.
	require.NoError(t, os.WriteFile(fragileFile, []byte("locally modified precious data"), 0o600))

	// Step 4: Delete remote.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/fragile.txt")

	// Step 4b: Wait for the remote delete to propagate (Graph API eventual
	// consistency). Without this, sync may not see the deletion and won't
	// detect the edit-delete conflict.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "fragile.txt", pollTimeout, "ls", "/"+testFolder)

	// Delta endpoint may lag behind REST item endpoints (ci_issues.md §17).
	// Re-sync until delta catches up and the edit-delete conflict is resolved.
	// The retry loop keeps using the normal sync path so conflict resolution is
	// exercised through durable engine-owned state, not a CLI-side bypass.
	var historyAfterResolution statusSyncStateJSON
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"sync should eventually detect and auto-resolve the edit-delete conflict",
		func(_ syncAttemptResult) bool {
			status, _, _, err := runStatusAllowError(t, cfgPath, env, "--history")
			if err != nil {
				return false
			}

			driveStatus := requireStatusDrive(t, status, drive)
			if driveStatus.SyncState == nil {
				return false
			}
			historyAfterResolution = *driveStatus.SyncState

			for _, entry := range historyAfterResolution.ConflictHistory {
				if strings.Contains(entry.Path, testFolder+"/fragile.txt") &&
					entry.ConflictType == "edit_delete" &&
					entry.Resolution == "keep_local" &&
					entry.ResolvedBy == "auto" {
					return true
				}
			}

			return false
		},
	)

	// Step 6: The owned edit-delete conflict is resolved even if unrelated
	// shared-drive churn causes other work in the same sync pass.

	// Step 7: Local file preserved with modified content.
	data, err := os.ReadFile(fragileFile)
	require.NoError(t, err)
	assert.Equal(t, "locally modified precious data", string(data))

	// Step 8: Remote file re-created (poll for eventual consistency).
	pollCLIWithConfigContains(t, opsCfgPath, nil, "fragile.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 9: Remote has the local content.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/fragile.txt")
	assert.Equal(t, "locally modified precious data", remoteContent)

	// Step 10: Conflict history shows the auto-resolved entry.
	require.Len(t, historyAfterResolution.ConflictHistory, 1)
	assert.Contains(t, historyAfterResolution.ConflictHistory[0].Path, "fragile.txt")
	assert.Equal(t, "edit_delete", historyAfterResolution.ConflictHistory[0].ConflictType)
	assert.Equal(t, "keep_local", historyAfterResolution.ConflictHistory[0].Resolution)
	assert.Equal(t, "auto", historyAfterResolution.ConflictHistory[0].ResolvedBy)

	// Step 11: No unresolved conflicts.
	assert.Empty(t, readStatusSyncState(t, cfgPath, env).Conflicts)

	// Step 12: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_ResolveAll exercises `resolve remote --all` with
// multiple edit-edit conflicts.
func TestE2E_Sync_ResolveAll(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-resall-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create two local files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-original"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-original"), 0o600))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 3: Modify both sides with different content.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-local-edit"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-local-edit"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/a.txt", "a-remote-edit")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/b.txt", "b-remote-edit")

	// Step 4: Bidirectional sync — 2 edit-edit conflicts.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")
	assert.Contains(t, stderr, "Conflicts:")

	// Step 5: Per-drive status reports both unresolved conflicts.
	statusBeforeResolveAll := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBeforeResolveAll.Conflicts, 2)

	// Step 6: Queue resolve remote --all, then normal sync executes the requests.
	queueConflictResolutionAndSync(t, cfgPath, env, "remote", "--all")

	// Step 7: No unresolved conflicts.
	assert.Empty(t, readStatusSyncState(t, cfgPath, env).Conflicts)

	// Step 8: Local files have remote content.
	aData, err := os.ReadFile(filepath.Join(localDir, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "a-remote-edit", string(aData))

	bData, err := os.ReadFile(filepath.Join(localDir, "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "b-remote-edit", string(bData))

	// Step 9: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal exercises EF12
// (create-create conflict) and resolve local with upload verification.
func TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-cc-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create sync dir but no initial sync (fresh — no baseline).
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	// Step 2: Create local file.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "collision.txt"), []byte("local version"), 0o600))

	// Step 3: Create remote file with different content at same path.
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/collision.txt", "remote version")

	// Step 4: Bidirectional sync — create-create conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")
	assert.Contains(t, stderr, "Conflicts:")

	// Step 5: Per-drive status reports the create-create conflict.
	statusBeforeKeepLocal := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBeforeKeepLocal.Conflicts, 1)
	assert.Contains(t, statusBeforeKeepLocal.Conflicts[0].Path, "collision.txt")
	assert.Equal(t, "create_create", statusBeforeKeepLocal.Conflicts[0].ConflictType)

	// Step 6: Conflict copy holds local content.
	matches, err := filepath.Glob(filepath.Join(localDir, "collision.conflict-*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected at least one conflict copy")

	conflictCopyData, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, "local version", string(conflictCopyData))

	// Step 7: Restore local version to original path (prep for keep-local).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "collision.txt"), []byte("local version"), 0o600))

	// Step 8: Queue resolve local, then normal sync executes and propagates it.
	queueConflictResolutionAndSync(t, cfgPath, env, "local", testFolder+"/collision.txt")

	// Step 9: No more conflicts.
	assert.Empty(t, readStatusSyncState(t, cfgPath, env).Conflicts)

	// Step 11: Remote should now have local version.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/collision.txt")
	assert.Equal(t, "local version", remoteContent)
}

// TestE2E_Sync_DeletePropagation exercises: EF6 (local delete→remote delete),
// EF8 (remote delete→local delete), EF10 (both deleted→cleanup),
// EF7 (local deleted+remote changed→download), ED6 (remote folder deleted).
func TestE2E_Sync_DeletePropagation(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-del-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local files.
	localDir := filepath.Join(syncDir, testFolder)
	subDir := filepath.Join(localDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep me"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-local.txt"), []byte("delete local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-remote.txt"), []byte("delete remote"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-both.txt"), []byte("delete both"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "redownload.txt"), []byte("original version"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested content"), 0o600))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Advance delta token past the upload so subsequent deletions are
	// visible via incremental delta (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Verify all files exist remotely (poll for eventual consistency).
	stdout, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, "keep.txt", pollTimeout, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "del-local.txt")
	assert.Contains(t, stdout, "del-remote.txt")
	assert.Contains(t, stdout, "del-both.txt")
	assert.Contains(t, stdout, "redownload.txt")
	assert.Contains(t, stdout, "sub")

	// Step 3: Set up delete scenarios.
	// EF6: Delete locally only.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-local.txt")))

	// EF8: Delete remotely only.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/del-remote.txt")

	// EF10: Delete both sides.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-both.txt")))
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/del-both.txt")

	// EF7: Delete locally + modify remotely.
	require.NoError(t, os.Remove(filepath.Join(localDir, "redownload.txt")))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/redownload.txt", "modified version")

	// ED6: Delete remote folder + file.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/sub/nested.txt")
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "-r", "/"+testFolder+"/sub")

	// Wait for remote deletes to propagate via REST before sync sees them via delta.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "del-remote.txt", pollTimeout, "ls", "/"+testFolder)
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "del-both.txt", pollTimeout, "ls", "/"+testFolder)
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "sub", pollTimeout, "ls", "/"+testFolder)

	// Step 4: Bidirectional sync — retry until delta catches up with ALL
	// remote deletions (ci_issues.md §17). The retry loop uses normal sync so
	// delete safety and durable approval state are exercised through the engine
	// boundary.
	// Check both del-remote.txt (EF8) and sub/ (ED6) since folder deletions
	// may propagate later than file deletions.
	// With cascade expansion (sync-planning.md §Folder Delete Cascade),
	// once delta reports the folder deletion, all children are deleted in
	// a single pass.
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"remote-only deletions should propagate locally after sync",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			_, errRemote := os.Stat(filepath.Join(localDir, "del-remote.txt"))
			_, errSub := os.Stat(filepath.Join(localDir, "sub"))
			return os.IsNotExist(errRemote) && os.IsNotExist(errSub)
		},
	)

	// Step 5: Assert remaining results.
	// EF6: del-local.txt gone remotely (poll for eventual consistency).
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "del-local", pollTimeout, "ls", "/"+testFolder)

	// EF10: del-both.txt gone everywhere.
	_, err := os.Stat(filepath.Join(localDir, "del-both.txt"))
	assert.True(t, os.IsNotExist(err), "del-both.txt should not exist locally")

	// EF7: redownload.txt re-downloaded with modified content.
	redownloadData, err := os.ReadFile(filepath.Join(localDir, "redownload.txt"))
	require.NoError(t, err)
	assert.Equal(t, "modified version", string(redownloadData))

	// EF1: keep.txt unchanged.
	keepData, err := os.ReadFile(filepath.Join(localDir, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(keepData))

	// Step 6: Re-sync should not mutate the converged test subtree, even if
	// other full-suite tests changed unrelated paths on the shared drive.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")
}

// Validates: R-6.2.5, R-6.4.1
// TestE2E_Sync_DeleteSafetyThreshold exercises S5 delete safety threshold.
// Creates 12 files, configures a low threshold (10) so that 12 deletions exceed
// it, verifies protection triggers, then approves the held deletes durably.
func TestE2E_Sync_DeleteSafetyThreshold(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "delete_safety_threshold = 10\n")
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-delsafe-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create 12 local files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	const fileCount = 12

	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(localDir, name), []byte(fmt.Sprintf("content %d", i)), 0o600))
	}

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Advance delta token past the upload. Upload-only mode skips remote
	// observation, so no delta token is saved. Download-only avoids delete
	// safety triggering from parallel test cleanup deletions.
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Poll for eventual consistency — verify all 12 files exist remotely
	// before proceeding to delete them.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "file-12.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 3: Delete all 12 files remotely.
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/"+name)
	}

	// Wait for remote deletes to propagate via REST before sync sees them via delta.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "file-01.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 4: Normal sync should trigger delete safety and record held rows.
	// Retry until delta catches up with all remote deletions (OneDrive
	// eventual consistency — delta may lag behind REST).
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"delete safety threshold should record held deletes",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}
			status := readStatusSyncState(t, cfgPath, env, "--verbose")
			return len(status.DeleteSafety) == fileCount
		},
	)

	// Step 5: Local files should still exist (no changes applied).
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		_, err := os.Stat(filepath.Join(localDir, name))
		assert.NoError(t, err, "local file %s should still exist after delete safety hold", name)
	}

	approvalOutput, _ := runCLIWithConfig(t, cfgPath, env, "resolve", "deletes")
	assert.Contains(t, approvalOutput, "Approved held deletes for this drive.")

	// Step 6: A normal sync should consume the durable approval and apply the deletes.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 7: All local files should be deleted.
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		_, err := os.Stat(filepath.Join(localDir, name))
		assert.True(t, os.IsNotExist(err), "local file %s should be deleted after sync", name)
	}

	// Step 8: Re-sync should leave the empty test folder state intact.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")
}

// TestE2E_Sync_DownloadOnlyIgnoresLocal exercises download-only mode:
// remote changes are applied, local-only changes are invisible.
func TestE2E_Sync_DownloadOnlyIgnoresLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-dlonly-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file and upload.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("initial"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 2: Modify both sides.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("local modification"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/shared.txt", "remote modification")

	// Step 3: Create new local-only file.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-new.txt"), []byte("local new file"), 0o600))

	// Step 4: Download-only sync.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	// Step 5: Local gets remote version (download-only overwrites local).
	sharedData, err := os.ReadFile(filepath.Join(localDir, "shared.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote modification", string(sharedData))

	// Step 6: local-new.txt still exists locally but was NOT uploaded.
	_, err = os.Stat(filepath.Join(localDir, "local-new.txt"))
	assert.NoError(t, err, "local-new.txt should still exist locally")

	// Verify NOT uploaded remotely.
	lsOut, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder)
	assert.NotContains(t, lsOut, "local-new.txt", "local-new.txt should not be uploaded in download-only mode")

	// Step 7: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_UploadOnlyIgnoresRemote exercises upload-only mode:
// local changes are uploaded, remote-only changes are invisible.
func TestE2E_Sync_UploadOnlyIgnoresRemote(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-uponly-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create remote file and download to establish baseline.
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-file.txt", "from remote")

	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Verify local download.
	localDir := filepath.Join(syncDir, testFolder)
	data, err := os.ReadFile(filepath.Join(localDir, "remote-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from remote", string(data))

	// Step 2: Modify remote, modify local, create new local file.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-file.txt", "modified remote v2")
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "remote-file.txt"), []byte("local edit v2"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "new-upload.txt"), []byte("new upload content"), 0o600))

	// Step 3: Upload-only sync.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Step 4: New local file uploaded (EF13, poll for eventual consistency).
	pollCLIWithConfigContains(t, opsCfgPath, nil, "new-upload.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 5: Local edit was uploaded (EF3 in upload-only).
	remoteContent := getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-file.txt")
	assert.Equal(t, "local edit v2", remoteContent)

	// Step 6: Local file still has local content (remote change was invisible).
	localData, err := os.ReadFile(filepath.Join(localDir, "remote-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local edit v2", string(localData))
}

// TestE2E_Sync_NestedFolderHierarchy exercises deep folder creation (ED5, ED3),
// file operations at multiple depths, and verify across the hierarchy.
func TestE2E_Sync_NestedFolderHierarchy(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-nested-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create deep local hierarchy.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "a", "b", "c"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "x", "y"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "c", "deep.txt"), []byte("deep content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "sibling.txt"), []byte("sibling content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "another.txt"), []byte("another content"), 0o600))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Verify deep files exist remotely (poll for eventual consistency).
	pollCLIWithConfigContains(t, opsCfgPath, nil, "deep.txt", pollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Step 3: Set up mixed changes.
	// New remote deeper folder.
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder+"/a/b/c/d")
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/a/b/c/d/deeper.txt", "deeper content")

	// Delete local sibling (EF6).
	require.NoError(t, os.Remove(filepath.Join(localDir, "a", "sibling.txt")))

	// New local deeper folder (ED5).
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "x", "y", "z"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "z", "new-leaf.txt"), []byte("leaf content"), 0o600))

	// Modify local file (EF3).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "another.txt"), []byte("modified another"), 0o600))

	// Step 4: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 5: Assert results.
	// Remote deeper.txt downloaded locally (ED3 + EF14).
	deeperData, err := os.ReadFile(filepath.Join(localDir, "a", "b", "c", "d", "deeper.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deeper content", string(deeperData))

	// sibling.txt removed remotely (EF6).
	siblingOut, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder+"/a")
	assert.NotContains(t, siblingOut, "sibling.txt")

	// new-leaf.txt uploaded remotely (ED5 + EF13).
	leafOut, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder+"/x/y/z")
	assert.Contains(t, leafOut, "new-leaf.txt")

	// another.txt uploaded with modification (EF3).
	remoteAnother := getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/x/y/another.txt")
	assert.Equal(t, "modified another", remoteAnother)

	// Step 6: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)

	// Step 7: Re-sync should leave the deep hierarchy unchanged even if the
	// shared live drive saw unrelated activity elsewhere.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")
}

// TestE2E_Sync_DryRunNonDestructive exercises dry-run: shows plan counts
// but applies zero side effects, then real sync applies changes.
func TestE2E_Sync_DryRunNonDestructive(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-dryrun-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync an initial file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("existing content"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 2: Set up pending changes.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "pending-upload.txt"), []byte("to upload"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/pending-download.txt", "to download")
	require.NoError(t, os.Remove(filepath.Join(localDir, "existing.txt")))

	// Step 3: Dry-run sync.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--dry-run")
	assert.Contains(t, stderr, "Dry run")

	// Step 4: Verify NO side effects.
	// pending-upload.txt NOT remotely uploaded.
	lsOut, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, "existing.txt", pollTimeout, "ls", "/"+testFolder)
	assert.NotContains(t, lsOut, "pending-upload.txt", "dry-run should not upload")

	// pending-download.txt NOT locally downloaded.
	_, err := os.Stat(filepath.Join(localDir, "pending-download.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run should not download")

	// existing.txt still present remotely (not deleted).
	assert.Contains(t, lsOut, "existing.txt", "dry-run should not delete remotely")

	// Step 5: Real sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 6: All changes applied (poll for eventual consistency).
	lsOut, _ = pollCLIWithConfigContains(t, opsCfgPath, nil, "pending-upload.txt", pollTimeout, "ls", "/"+testFolder)
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "existing.txt", pollTimeout, "ls", "/"+testFolder)
	assert.Contains(t, lsOut, "pending-upload.txt")

	dlData, err := os.ReadFile(filepath.Join(localDir, "pending-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "to download", string(dlData))

	// Step 7: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_ConvergentEdit exercises EF4 (convergent edit — same hash
// both sides) and EF11 (convergent create). The owned subtree should remain
// converged without conflicts even if unrelated full-suite remote churn
// causes transfers elsewhere on the shared drive.
func TestE2E_Sync_ConvergentEdit(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-conv-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync baseline file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Advance delta token past the upload so the subsequent bidirectional
	// sync uses incremental delta, not full enumeration (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Step 2: Modify both sides to the SAME content (EF4).
	newContent := "convergent new content"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte(newContent), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/converge-edit.txt", newContent)

	// Step 3: Create new file on both sides with same content (EF11).
	freshContent := "fresh convergent"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-create.txt"), []byte(freshContent), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/converge-create.txt", freshContent)

	// Step 4: Bidirectional sync — convergent detection.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")

	// Convergent updates detected (no data transfer needed).
	assert.Contains(t, stderr, "Synced updates:")
	assert.NotContains(t, stderr, "Conflicts:")

	editData, err := os.ReadFile(filepath.Join(localDir, "converge-edit.txt"))
	require.NoError(t, err)
	assert.Equal(t, newContent, string(editData))

	createData, err := os.ReadFile(filepath.Join(localDir, "converge-create.txt"))
	require.NoError(t, err)
	assert.Equal(t, freshContent, string(createData))

	assert.Equal(t, newContent, getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/converge-edit.txt"))
	assert.Equal(t, freshContent, getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/converge-create.txt"))

	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")

	// Step 5: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_InternalBaselineVerificationDetectsTampering exercises the
// internal verifier detecting hash mismatches and missing files after local
// tampering without syncing.
func TestE2E_Sync_InternalBaselineVerificationDetectsTampering(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-tamp-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "good.txt"), []byte("good content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "tampered.txt"), []byte("original content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "missing.txt"), []byte("will be removed"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 2: Internal baseline verification is clean initially.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)

	// Step 3: Tamper locally WITHOUT syncing.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "tampered.txt"), []byte("TAMPERED CONTENT"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(localDir, "missing.txt")))

	// Step 4: Internal verification should detect exactly the tampered paths.
	report, err = verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 2)
	assert.Equal(t, 1, report.Verified)

	mismatchByPath := make(map[string]string, len(report.Mismatches))
	for _, mismatch := range report.Mismatches {
		mismatchByPath[mismatch.Path] = mismatch.Status
	}

	assert.Equal(t, "hash_mismatch", mismatchByPath[path.Join(testFolder, "tampered.txt")])
	assert.Equal(t, "missing", mismatchByPath[path.Join(testFolder, "missing.txt")])
}

// TestE2E_Sync_ResolveDryRun exercises resolve --dry-run: shows what would
// happen without actually resolving the conflict, then resolves for real.
func TestE2E_Sync_ResolveDryRun(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-sync-resdry-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file and upload.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	conflictFile := filepath.Join(localDir, "dryrun-conflict.txt")
	require.NoError(t, os.WriteFile(conflictFile, []byte("original v1"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 2: Modify both sides to create edit-edit conflict.
	require.NoError(t, os.WriteFile(conflictFile, []byte("local edit v2"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/dryrun-conflict.txt", "remote edit v2")

	// Step 3: Bidirectional sync — should detect conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")
	assert.Contains(t, stderr, "Conflicts:")

	// Step 4: Per-drive status reports the unresolved conflict.
	statusBeforeDryRun := readStatusSyncState(t, cfgPath, env)
	require.Len(t, statusBeforeDryRun.Conflicts, 1)
	assert.Contains(t, statusBeforeDryRun.Conflicts[0].Path, "dryrun-conflict.txt")
	assert.Equal(t, "edit_edit", statusBeforeDryRun.Conflicts[0].ConflictType)

	// Step 5: Resolve --dry-run local.
	_, stderr = runCLIWithConfig(t, cfgPath, env, "resolve", "local", testFolder+"/dryrun-conflict.txt", "--dry-run")
	assert.Contains(t, stderr, "Would resolve")

	// Step 6: Conflict should still exist (dry-run didn't resolve it).
	assert.Len(t, readStatusSyncState(t, cfgPath, env).Conflicts, 1, "conflict should remain after dry-run resolve")

	// Step 7: Queue for real, then normal sync executes the request.
	queueConflictResolutionAndSync(t, cfgPath, env, "local", testFolder+"/dryrun-conflict.txt")

	// Step 8: No more conflicts.
	assert.Empty(t, readStatusSyncState(t, cfgPath, env).Conflicts)
}
