//go:build e2e && e2e_full

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
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
	testFolder := fmt.Sprintf("onedrive-go-e2e-edge-%d", time.Now().UnixNano())

	// Cleanup at the end — delete the test folder (best-effort).
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Create the test folder first.
	runCLI(t, "mkdir", "/"+testFolder)

	t.Run("large_file_upload_download", func(t *testing.T) {
		testLargeFileUploadDownload(t, testFolder)
	})

	t.Run("unicode_filename", func(t *testing.T) {
		testUnicodeFilename(t, testFolder)
	})

	t.Run("spaces_in_filename", func(t *testing.T) {
		testSpacesInFilename(t, testFolder)
	})

	t.Run("concurrent_uploads", func(t *testing.T) {
		testConcurrentUploads(t, testFolder)
	})
}

// testLargeFileUploadDownload generates a 5 MiB file (exceeding the 4 MB
// simple-upload threshold) to exercise the resumable upload path.
func testLargeFileUploadDownload(t *testing.T, testFolder string) {
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
	_, stderr := runCLI(t, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// Stat — verify the file size matches.
	stdout, _ := runCLI(t, "stat", remotePath)
	assert.Contains(t, stdout, fmt.Sprintf("%d bytes", fileSize))

	// Download and verify byte-for-byte content integrity.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "large-file.bin")

	_, stderr = runCLI(t, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, data, downloaded, "downloaded file content does not match uploaded data")

	// Cleanup the remote file.
	_, stderr = runCLI(t, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testUnicodeFilename verifies that files with non-ASCII (Japanese) characters
// in the filename can be uploaded, listed, downloaded, and deleted.
func testUnicodeFilename(t *testing.T, testFolder string) {
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
	_, stderr := runCLI(t, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// List the folder and confirm the unicode name appears.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, remoteName)

	// Download and verify content.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "downloaded-unicode.txt")

	_, stderr = runCLI(t, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)

	// Cleanup.
	_, stderr = runCLI(t, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testSpacesInFilename verifies that filenames containing spaces are handled
// correctly through upload, stat, download, and delete.
func testSpacesInFilename(t *testing.T, testFolder string) {
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
	_, stderr := runCLI(t, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")

	// Stat — verify the name appears.
	stdout, _ := runCLI(t, "stat", remotePath)
	assert.Contains(t, stdout, remoteName)

	// Download and verify content.
	downloadDir := t.TempDir()
	localPath := filepath.Join(downloadDir, "downloaded-spaces.txt")

	_, stderr = runCLI(t, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)

	// Cleanup.
	_, stderr = runCLI(t, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
}

// testFile holds local and remote paths for a file used in concurrent upload tests.
type testFile struct {
	localPath  string
	remoteName string
}

// testConcurrentUploads verifies that multiple files can be uploaded in
// parallel without errors or data corruption.
func testConcurrentUploads(t *testing.T, testFolder string) {
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

	// Upload all files in parallel. We use exec.Command directly instead
	// of runCLI because t.Fatalf panics when called from non-test goroutines.
	errCh := make(chan error, fileCount)

	for i := range files {
		go func(f testFile) {
			remotePath := "/" + testFolder + "/" + f.remoteName
			fullArgs := []string{"--drive", drive, "put", f.localPath, remotePath}
			cmd := exec.Command(binaryPath, fullArgs...)

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

	// Verify all files are present in the folder listing.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	for _, f := range files {
		assert.Contains(t, stdout, f.remoteName,
			"expected %s in folder listing", f.remoteName)
	}

	// Cleanup all uploaded files.
	for _, f := range files {
		remotePath := "/" + testFolder + "/" + f.remoteName
		_, stderr := runCLI(t, "rm", remotePath)
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
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-bidi-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local files in a docs/ subfolder.
	docsDir := filepath.Join(syncDir, testFolder, "docs")
	require.NoError(t, os.MkdirAll(docsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "readme.txt"), []byte("readme content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "notes.txt"), []byte("notes content"), 0o644))

	// Step 2: Upload-only sync to establish baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 3: Assert files exist remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder+"/docs")
	assert.Contains(t, stdout, "readme.txt")
	assert.Contains(t, stdout, "notes.txt")

	// Step 4: Create new local folder + file (EF13 + ED5).
	localOnlyDir := filepath.Join(syncDir, testFolder, "local-only")
	require.NoError(t, os.MkdirAll(localOnlyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localOnlyDir, "stuff.txt"), []byte("local stuff"), 0o644))

	// Step 5: Create new remote folder + file (EF14 + ED3).
	runCLI(t, "mkdir", "/"+testFolder+"/data")
	putRemoteFile(t, "/"+testFolder+"/data/info.txt", "remote info data")

	// Step 6: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 7: Assert merge results.
	// stuff.txt uploaded remotely (EF13 + ED5).
	stdout, _ = runCLI(t, "ls", "/"+testFolder+"/local-only")
	assert.Contains(t, stdout, "stuff.txt")

	// info.txt downloaded locally (EF14 + ED3).
	infoData, err := os.ReadFile(filepath.Join(syncDir, testFolder, "data", "info.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote info data", string(infoData))

	// readme.txt unchanged (EF1).
	readmeData, err := os.ReadFile(filepath.Join(syncDir, testFolder, "docs", "readme.txt"))
	require.NoError(t, err)
	assert.Equal(t, "readme content", string(readmeData))

	// Step 8: Verify integrity.
	stdout, _ = runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
	assert.Contains(t, stdout, "All files verified successfully.")

	// Step 9: Re-sync is idempotent.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected")
}

// TestE2E_Sync_EditEditConflict_ResolveKeepRemote exercises EF5 (edit-edit
// conflict), conflict copy creation, and resolve --keep-remote.
func TestE2E_Sync_EditEditConflict_ResolveKeepRemote(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-ee-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	conflictFile := filepath.Join(localDir, "conflict-file.txt")
	require.NoError(t, os.WriteFile(conflictFile, []byte("original v1"), 0o644))

	// Step 2: Upload-only sync.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 3: Modify local.
	require.NoError(t, os.WriteFile(conflictFile, []byte("local edit v2"), 0o644))

	// Step 4: Modify remote with different content.
	putRemoteFile(t, "/"+testFolder+"/conflict-file.txt", "remote edit v2")

	// Step 5: Bidirectional sync — should detect conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 6: Conflict reported in sync output.
	assert.Contains(t, stderr, "Conflicts:")

	// Step 7: List conflicts — type is edit_edit.
	stdout, _ := runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "conflict-file.txt")
	assert.Contains(t, stdout, "edit_edit")

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

	// Step 11: Resolve --keep-remote.
	_, stderr = runCLIWithConfig(t, cfgPath, "resolve", testFolder+"/conflict-file.txt", "--keep-remote")

	// Step 12: Resolved message.
	assert.Contains(t, stderr, "Resolved")

	// Step 13: No more conflicts.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "No unresolved conflicts")

	// Step 14: Verify passes.
	stdout, _ = runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
}

// TestE2E_Sync_EditDeleteConflict exercises EF9 (edit-delete conflict)
// auto-resolve: local edit wins. The modified local file is uploaded to
// re-create the remote, and a resolved conflict is recorded in history.
func TestE2E_Sync_EditDeleteConflict(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-ed-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	fragileFile := filepath.Join(localDir, "fragile.txt")
	require.NoError(t, os.WriteFile(fragileFile, []byte("precious data"), 0o644))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 3: Modify local.
	require.NoError(t, os.WriteFile(fragileFile, []byte("locally modified precious data"), 0o644))

	// Step 4: Delete remote.
	runCLI(t, "rm", "/"+testFolder+"/fragile.txt")

	// Step 5: Bidirectional sync — edit-delete auto-resolved by uploading local.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 6: Sync succeeded (auto-resolved, no failures).
	assert.NotContains(t, stderr, "Failed:")

	// Step 7: Local file preserved with modified content.
	data, err := os.ReadFile(fragileFile)
	require.NoError(t, err)
	assert.Equal(t, "locally modified precious data", string(data))

	// Step 8: Remote file re-created.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "fragile.txt")

	// Step 9: Remote has the local content.
	remoteContent := getRemoteFile(t, "/"+testFolder+"/fragile.txt")
	assert.Equal(t, "locally modified precious data", remoteContent)

	// Step 10: Conflict history shows auto-resolved entry.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts", "--history")
	assert.Contains(t, stdout, "fragile.txt")
	assert.Contains(t, stdout, "edit_delete")
	assert.Contains(t, stdout, "keep_local")
	assert.Contains(t, stdout, "auto")

	// Step 11: No unresolved conflicts.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "No unresolved conflicts")

	// Step 12: Verify passes.
	stdout, _ = runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
}

// TestE2E_Sync_ResolveAll exercises 'resolve --all --keep-remote' with
// multiple edit-edit conflicts.
func TestE2E_Sync_ResolveAll(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-resall-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create two local files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-original"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-original"), 0o644))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 3: Modify both sides with different content.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("a-local-edit"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("b-local-edit"), 0o644))
	putRemoteFile(t, "/"+testFolder+"/a.txt", "a-remote-edit")
	putRemoteFile(t, "/"+testFolder+"/b.txt", "b-remote-edit")

	// Step 4: Bidirectional sync — 2 edit-edit conflicts.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "Conflicts:")

	// Step 5: List conflicts — both present.
	stdout, _ := runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "a.txt")
	assert.Contains(t, stdout, "b.txt")
	assert.Contains(t, stdout, "edit_edit")

	// Step 6: Resolve --all --keep-remote.
	_, stderr = runCLIWithConfig(t, cfgPath, "resolve", "--all", "--keep-remote")
	assert.Contains(t, stderr, "Resolved")

	// Step 7: No unresolved conflicts.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "No unresolved conflicts")

	// Step 8: Local files have remote content.
	aData, err := os.ReadFile(filepath.Join(localDir, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "a-remote-edit", string(aData))

	bData, err := os.ReadFile(filepath.Join(localDir, "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "b-remote-edit", string(bData))

	// Step 9: Verify passes.
	stdout, _ = runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
}

// TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal exercises EF12
// (create-create conflict) and resolve --keep-local with upload verification.
func TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-cc-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create sync dir but no initial sync (fresh — no baseline).
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	// Step 2: Create local file.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "collision.txt"), []byte("local version"), 0o644))

	// Step 3: Create remote file with different content at same path.
	runCLI(t, "mkdir", "/"+testFolder)
	putRemoteFile(t, "/"+testFolder+"/collision.txt", "remote version")

	// Step 4: Bidirectional sync — create-create conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "Conflicts:")

	// Step 5: Verify conflict type.
	stdout, _ := runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "collision.txt")
	assert.Contains(t, stdout, "create_create")

	// Step 6: Conflict copy holds local content.
	matches, err := filepath.Glob(filepath.Join(localDir, "collision.conflict-*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected at least one conflict copy")

	conflictCopyData, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, "local version", string(conflictCopyData))

	// Step 7: Restore local version to original path (prep for keep-local).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "collision.txt"), []byte("local version"), 0o644))

	// Step 8: Resolve --keep-local.
	_, stderr = runCLIWithConfig(t, cfgPath, "resolve", testFolder+"/collision.txt", "--keep-local")
	assert.Contains(t, stderr, "Resolved")

	// Step 9: No more conflicts.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "No unresolved conflicts")

	// Step 10: Sync to propagate local version upstream.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 11: Remote should now have local version.
	remoteContent := getRemoteFile(t, "/"+testFolder+"/collision.txt")
	assert.Equal(t, "local version", remoteContent)
}

// TestE2E_Sync_DeletePropagation exercises: EF6 (local delete→remote delete),
// EF8 (remote delete→local delete), EF10 (both deleted→cleanup),
// EF7 (local deleted+remote changed→download), ED6 (remote folder deleted).
func TestE2E_Sync_DeletePropagation(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-del-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local files.
	localDir := filepath.Join(syncDir, testFolder)
	subDir := filepath.Join(localDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep me"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-local.txt"), []byte("delete local"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-remote.txt"), []byte("delete remote"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "del-both.txt"), []byte("delete both"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "redownload.txt"), []byte("original version"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested content"), 0o644))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Verify all files exist remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "keep.txt")
	assert.Contains(t, stdout, "del-local.txt")
	assert.Contains(t, stdout, "del-remote.txt")
	assert.Contains(t, stdout, "del-both.txt")
	assert.Contains(t, stdout, "redownload.txt")
	assert.Contains(t, stdout, "sub")

	// Step 3: Set up delete scenarios.
	// EF6: Delete locally only.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-local.txt")))

	// EF8: Delete remotely only.
	runCLI(t, "rm", "/"+testFolder+"/del-remote.txt")

	// EF10: Delete both sides.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-both.txt")))
	runCLI(t, "rm", "/"+testFolder+"/del-both.txt")

	// EF7: Delete locally + modify remotely.
	require.NoError(t, os.Remove(filepath.Join(localDir, "redownload.txt")))
	putRemoteFile(t, "/"+testFolder+"/redownload.txt", "modified version")

	// ED6: Delete remote folder + file.
	runCLI(t, "rm", "/"+testFolder+"/sub/nested.txt")
	runCLI(t, "rm", "/"+testFolder+"/sub")

	// Step 4: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 5: Assert results.
	// EF6: del-local.txt gone remotely.
	lsOut := runCLIExpectError(t, "ls", "/"+testFolder+"/del-local.txt")
	assert.Contains(t, lsOut, "del-local")

	// EF8: del-remote.txt gone locally.
	_, err := os.Stat(filepath.Join(localDir, "del-remote.txt"))
	assert.True(t, os.IsNotExist(err), "del-remote.txt should not exist locally")

	// EF10: del-both.txt gone everywhere.
	_, err = os.Stat(filepath.Join(localDir, "del-both.txt"))
	assert.True(t, os.IsNotExist(err), "del-both.txt should not exist locally")

	// EF7: redownload.txt re-downloaded with modified content.
	redownloadData, err := os.ReadFile(filepath.Join(localDir, "redownload.txt"))
	require.NoError(t, err)
	assert.Equal(t, "modified version", string(redownloadData))

	// ED6: sub/ folder gone locally.
	_, err = os.Stat(filepath.Join(localDir, "sub"))
	assert.True(t, os.IsNotExist(err), "sub/ folder should not exist locally")

	// EF1: keep.txt unchanged.
	keepData, err := os.ReadFile(filepath.Join(localDir, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(keepData))

	// Step 6: Re-sync is idempotent.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected")
}

// TestE2E_Sync_BigDeleteProtection exercises S5 big-delete protection and
// the --force override. Creates 12 files (above MinItems=10 threshold),
// deletes all remotely (100% > 50% MaxPercent), verifies protection triggers.
func TestE2E_Sync_BigDeleteProtection(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-bigdel-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create 12 local files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	const fileCount = 12

	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(localDir, name), []byte(fmt.Sprintf("content %d", i)), 0o644))
	}

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 3: Delete all 12 files remotely.
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		runCLI(t, "rm", "/"+testFolder+"/"+name)
	}

	// Step 4: Sync without --force — big-delete protection should trigger.
	_, stderr, syncErr := runCLIWithConfigAllowError(t, cfgPath, "sync")
	require.Error(t, syncErr, "sync should fail due to big-delete protection")
	assert.Contains(t, stderr, "big-delete")

	// Step 5: Local files should still exist (no changes applied).
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		_, err := os.Stat(filepath.Join(localDir, name))
		assert.NoError(t, err, "local file %s should still exist after big-delete protection", name)
	}

	// Step 6: Sync with --force — should succeed.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 7: All local files should be deleted.
	for i := 1; i <= fileCount; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		_, err := os.Stat(filepath.Join(localDir, name))
		assert.True(t, os.IsNotExist(err), "local file %s should be deleted after --force sync", name)
	}

	// Step 8: Re-sync is idempotent.
	_, stderr = runCLIWithConfig(t, cfgPath, "sync")
	assert.Contains(t, stderr, "No changes detected")
}

// TestE2E_Sync_DownloadOnlyIgnoresLocal exercises download-only mode:
// remote changes are applied, local-only changes are invisible.
func TestE2E_Sync_DownloadOnlyIgnoresLocal(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-dlonly-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create local file and upload.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("initial"), 0o644))

	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 2: Modify both sides.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("local modification"), 0o644))
	putRemoteFile(t, "/"+testFolder+"/shared.txt", "remote modification")

	// Step 3: Create new local-only file.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-new.txt"), []byte("local new file"), 0o644))

	// Step 4: Download-only sync.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--download-only", "--force")
	assert.Contains(t, stderr, "Mode: download-only")

	// Step 5: Local gets remote version (download-only overwrites local).
	sharedData, err := os.ReadFile(filepath.Join(localDir, "shared.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote modification", string(sharedData))

	// Step 6: local-new.txt still exists locally but was NOT uploaded.
	_, err = os.Stat(filepath.Join(localDir, "local-new.txt"))
	assert.NoError(t, err, "local-new.txt should still exist locally")

	// Verify NOT uploaded remotely.
	lsOut, _ := runCLI(t, "ls", "/"+testFolder)
	assert.NotContains(t, lsOut, "local-new.txt", "local-new.txt should not be uploaded in download-only mode")

	// Step 7: Verify.
	stdout, _ := runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
}

// TestE2E_Sync_UploadOnlyIgnoresRemote exercises upload-only mode:
// local changes are uploaded, remote-only changes are invisible.
func TestE2E_Sync_UploadOnlyIgnoresRemote(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-uponly-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create remote file and download to establish baseline.
	runCLI(t, "mkdir", "/"+testFolder)
	putRemoteFile(t, "/"+testFolder+"/remote-file.txt", "from remote")

	runCLIWithConfig(t, cfgPath, "sync", "--download-only", "--force")

	// Verify local download.
	localDir := filepath.Join(syncDir, testFolder)
	data, err := os.ReadFile(filepath.Join(localDir, "remote-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from remote", string(data))

	// Step 2: Modify remote, modify local, create new local file.
	putRemoteFile(t, "/"+testFolder+"/remote-file.txt", "modified remote v2")
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "remote-file.txt"), []byte("local edit v2"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "new-upload.txt"), []byte("new upload content"), 0o644))

	// Step 3: Upload-only sync.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Step 4: New local file uploaded (EF13).
	lsOut, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, lsOut, "new-upload.txt")

	// Step 5: Local edit was uploaded (EF3 in upload-only).
	remoteContent := getRemoteFile(t, "/"+testFolder+"/remote-file.txt")
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
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-nested-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create deep local hierarchy.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "a", "b", "c"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "x", "y"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "c", "deep.txt"), []byte("deep content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "sibling.txt"), []byte("sibling content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "another.txt"), []byte("another content"), 0o644))

	// Step 2: Upload baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Verify deep files exist remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder+"/a/b/c")
	assert.Contains(t, stdout, "deep.txt")

	// Step 3: Set up mixed changes.
	// New remote deeper folder.
	runCLI(t, "mkdir", "/"+testFolder+"/a/b/c/d")
	putRemoteFile(t, "/"+testFolder+"/a/b/c/d/deeper.txt", "deeper content")

	// Delete local sibling (EF6).
	require.NoError(t, os.Remove(filepath.Join(localDir, "a", "sibling.txt")))

	// New local deeper folder (ED5).
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "x", "y", "z"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "z", "new-leaf.txt"), []byte("leaf content"), 0o644))

	// Modify local file (EF3).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "another.txt"), []byte("modified another"), 0o644))

	// Step 4: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 5: Assert results.
	// Remote deeper.txt downloaded locally (ED3 + EF14).
	deeperData, err := os.ReadFile(filepath.Join(localDir, "a", "b", "c", "d", "deeper.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deeper content", string(deeperData))

	// sibling.txt removed remotely (EF6).
	siblingOut, _ := runCLI(t, "ls", "/"+testFolder+"/a")
	assert.NotContains(t, siblingOut, "sibling.txt")

	// new-leaf.txt uploaded remotely (ED5 + EF13).
	leafOut, _ := runCLI(t, "ls", "/"+testFolder+"/x/y/z")
	assert.Contains(t, leafOut, "new-leaf.txt")

	// another.txt uploaded with modification (EF3).
	remoteAnother := getRemoteFile(t, "/"+testFolder+"/x/y/another.txt")
	assert.Equal(t, "modified another", remoteAnother)

	// Step 6: Verify.
	stdout, _ = runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
	assert.Contains(t, stdout, "All files verified successfully.")

	// Step 7: Re-sync is idempotent.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected")
}

// TestE2E_Sync_DryRunNonDestructive exercises dry-run: shows plan counts
// but applies zero side effects, then real sync applies changes.
func TestE2E_Sync_DryRunNonDestructive(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-dryrun-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync an initial file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("existing content"), 0o644))

	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 2: Set up pending changes.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "pending-upload.txt"), []byte("to upload"), 0o644))
	putRemoteFile(t, "/"+testFolder+"/pending-download.txt", "to download")
	require.NoError(t, os.Remove(filepath.Join(localDir, "existing.txt")))

	// Step 3: Dry-run sync.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--dry-run", "--force")
	assert.Contains(t, stderr, "Dry run")

	// Step 4: Verify NO side effects.
	// pending-upload.txt NOT remotely uploaded.
	lsOut, _ := runCLI(t, "ls", "/"+testFolder)
	assert.NotContains(t, lsOut, "pending-upload.txt", "dry-run should not upload")

	// pending-download.txt NOT locally downloaded.
	_, err := os.Stat(filepath.Join(localDir, "pending-download.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run should not download")

	// existing.txt still present remotely (not deleted).
	assert.Contains(t, lsOut, "existing.txt", "dry-run should not delete remotely")

	// Step 5: Real sync.
	runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Step 6: All changes applied.
	lsOut, _ = runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, lsOut, "pending-upload.txt", "real sync should upload")
	assert.Contains(t, lsOut, "pending-download.txt")
	assert.NotContains(t, lsOut, "existing.txt", "real sync should propagate delete")

	dlData, err := os.ReadFile(filepath.Join(localDir, "pending-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "to download", string(dlData))

	// Step 7: Verify.
	stdout, _ := runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
	assert.Contains(t, stdout, "All files verified successfully.")
}

// TestE2E_Sync_ConvergentEdit exercises EF4 (convergent edit — same hash
// both sides) and EF11 (convergent create). No data transfer should occur.
func TestE2E_Sync_ConvergentEdit(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-conv-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync baseline file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte("original"), 0o644))

	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 2: Modify both sides to the SAME content (EF4).
	newContent := "convergent new content"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte(newContent), 0o644))
	putRemoteFile(t, "/"+testFolder+"/converge-edit.txt", newContent)

	// Step 3: Create new file on both sides with same content (EF11).
	freshContent := "fresh convergent"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-create.txt"), []byte(freshContent), 0o644))
	putRemoteFile(t, "/"+testFolder+"/converge-create.txt", freshContent)

	// Step 4: Bidirectional sync — convergent detection.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")

	// Convergent updates detected (no data transfer needed).
	assert.Contains(t, stderr, "Synced updates:")

	// No downloads or uploads (hashes match — no transfer).
	assert.NotContains(t, stderr, "Downloads:")
	assert.NotContains(t, stderr, "Uploads:")

	// Step 5: Verify.
	stdout, _ := runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
	assert.Contains(t, stdout, "All files verified successfully.")
}

// TestE2E_Sync_VerifyDetectsTampering exercises verify detecting hash
// mismatches and missing files after local tampering without syncing.
func TestE2E_Sync_VerifyDetectsTampering(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-tamp-%d", time.Now().UnixNano())

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create and sync files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "good.txt"), []byte("good content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "tampered.txt"), []byte("original content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "missing.txt"), []byte("will be removed"), 0o644))

	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 2: Verify all good initially.
	stdout, _ := runCLIWithConfig(t, cfgPath, "verify")
	assert.Contains(t, stdout, "Verified")
	assert.Contains(t, stdout, "All files verified successfully.")

	// Step 3: Tamper locally WITHOUT syncing.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "tampered.txt"), []byte("TAMPERED CONTENT"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(localDir, "missing.txt")))

	// Step 4: Verify should detect mismatches (exit code ≠ 0).
	stdout, _, verifyErr := runCLIWithConfigAllowError(t, cfgPath, "verify")
	require.Error(t, verifyErr, "verify should fail when local files are tampered")

	// Step 5: Assert mismatch report.
	assert.Contains(t, stdout, "Mismatches:")
	assert.Contains(t, stdout, "tampered.txt")
	assert.Contains(t, stdout, "hash_mismatch")
	assert.Contains(t, stdout, "missing.txt")
	assert.Contains(t, stdout, "missing")

	// good.txt should not appear in mismatches.
	// Split stdout by Mismatches section — good.txt should only be in Verified count.
	lines := strings.Split(stdout, "\n")
	inMismatchSection := false
	for _, line := range lines {
		if strings.Contains(line, "Mismatches:") {
			inMismatchSection = true
			continue
		}
		if inMismatchSection {
			assert.NotContains(t, line, "good.txt", "good.txt should not be in mismatch table")
		}
	}
}
