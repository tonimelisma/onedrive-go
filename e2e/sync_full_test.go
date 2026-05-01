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

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
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
	mkdirRemoteFolder(t, opsCfgPath, nil, "/"+testFolder)
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
	waitForRemoteReadContains(t, opsCfgPath, nil, "", fmt.Sprintf("%d bytes", fileSize), pollTimeout, "stat", remotePath)

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
	waitForRemoteReadContains(t, opsCfgPath, nil, "", remoteName, pollTimeout, "ls", "/"+testFolder)

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
	waitForRemoteReadContains(t, opsCfgPath, nil, "", remoteName, pollTimeout, "stat", remotePath)

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
	stdout, _ := waitForRemoteReadContains(t, opsCfgPath, nil, "", files[len(files)-1].remoteName, pollTimeout, "ls", "/"+testFolder)
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

// TestE2E_Sync_BidirectionalMerge exercises unchanged files, local edits,
// local creates, remote creates, folder creation, verification, and idempotent
// re-sync.
func TestE2E_Sync_BidirectionalMerge(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-bidi-%d", time.Now().UnixNano())

	// Step 1: Create local files in a docs/ subfolder.
	docsDir := filepath.Join(syncDir, testFolder, "docs")
	require.NoError(t, os.MkdirAll(docsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "readme.txt"), []byte("readme content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(docsDir, "notes.txt"), []byte("notes content"), 0o600))

	// Step 2: Upload-only sync to establish baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 3: Assert files exist remotely (poll for eventual consistency).
	stdout, _ := waitForRemoteReadContains(t, cfgPath, env, "", "readme.txt", pollTimeout, "ls", "/"+testFolder+"/docs")
	assert.Contains(t, stdout, "notes.txt")

	// Step 4: Create a new local folder and file.
	localOnlyDir := filepath.Join(syncDir, testFolder, "local-only")
	require.NoError(t, os.MkdirAll(localOnlyDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localOnlyDir, "stuff.txt"), []byte("local stuff"), 0o600))

	// Step 5: Create a new remote folder and file.
	mkdirRemoteFolder(t, cfgPath, env, "/"+testFolder+"/data")
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/data/info.txt", "remote info data")

	// Step 6: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 7: Assert merge results.
	// stuff.txt uploaded remotely.
	stdout, _ = runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder+"/local-only")
	assert.Contains(t, stdout, "stuff.txt")

	// info.txt downloaded locally.
	infoData, err := os.ReadFile(filepath.Join(syncDir, testFolder, "data", "info.txt"))
	require.NoError(t, err)
	assert.Equal(t, "remote info data", string(infoData))

	// readme.txt unchanged.
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

// TestE2E_Sync_EditDeleteConflict exercises local-edit plus remote-delete:
// local content stays canonical and recreates the remote item through the
// normal sync path.
func TestE2E_Sync_EditDeleteConflict(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-ed-%d", time.Now().UnixNano())

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
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/fragile.txt")

	// Step 4b: Wait for the remote delete to propagate (Graph API eventual
	// consistency). Without this, sync may not see the deletion and won't
	// detect the edit-delete conflict.
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "fragile.txt", "ls", "/"+testFolder)

	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"sync should eventually recreate the remote item from the local edit",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			data, err := os.ReadFile(fragileFile)
			if err != nil || string(data) != "locally modified precious data" {
				return false
			}
			stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, "stat", "/"+testFolder+"/fragile.txt")
			if err != nil {
				t.Logf("waiting for recreated remote file\nstdout: %s\nstderr: %s", stdout, stderr)
				return false
			}

			return getRemoteFile(t, cfgPath, env, "/"+testFolder+"/fragile.txt") == "locally modified precious data"
		},
	)

	matches := conflictCopiesForPath(t, fragileFile)
	assert.Empty(t, matches, "edit-delete recreate upload should not create a conflict copy")

	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_DeletePropagation exercises local delete, remote delete,
// both-sides delete, local-delete plus remote-change download, and remote
// folder delete propagation.
func TestE2E_Sync_DeletePropagation(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-del-%d", time.Now().UnixNano())

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
	stdout, _ := waitForRemoteReadContains(t, cfgPath, env, "", "keep.txt", pollTimeout, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "del-local.txt")
	assert.Contains(t, stdout, "del-remote.txt")
	assert.Contains(t, stdout, "del-both.txt")
	assert.Contains(t, stdout, "redownload.txt")
	assert.Contains(t, stdout, "sub")

	// Step 3: Set up delete scenarios.
	// Delete locally only.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-local.txt")))

	// Delete remotely only.
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/del-remote.txt")

	// Delete both sides.
	require.NoError(t, os.Remove(filepath.Join(localDir, "del-both.txt")))
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/del-both.txt")

	// Delete locally and modify remotely.
	require.NoError(t, os.Remove(filepath.Join(localDir, "redownload.txt")))
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/redownload.txt", "modified version")

	// Delete a remote folder and file.
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/sub/nested.txt")
	runCLIWithConfig(t, cfgPath, env, "rm", "-r", "/"+testFolder+"/sub")

	// Wait for remote deletes to propagate via REST before sync sees them via delta.
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "del-remote.txt", "ls", "/"+testFolder)
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "del-both.txt", "ls", "/"+testFolder)
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "sub", "ls", "/"+testFolder)

	// Step 4: Bidirectional sync — retry until delta catches up with ALL
	// remote deletions (ci_issues.md §17). The retry loop uses normal sync so
	// delete safety is exercised through the engine boundary.
	// Check both del-remote.txt and sub/ since folder deletions may propagate
	// later than file deletions. Planner-visible pruning makes descendants
	// reconcile as ordinary delete rows once delta reports the folder deletion.
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
	// del-local.txt gone remotely (poll for eventual consistency).
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "del-local", "ls", "/"+testFolder)

	// del-both.txt gone everywhere.
	_, err := os.Stat(filepath.Join(localDir, "del-both.txt"))
	assert.True(t, os.IsNotExist(err), "del-both.txt should not exist locally")

	// redownload.txt re-downloaded with modified content.
	redownloadData, err := os.ReadFile(filepath.Join(localDir, "redownload.txt"))
	require.NoError(t, err)
	assert.Equal(t, "modified version", string(redownloadData))

	// keep.txt unchanged.
	keepData, err := os.ReadFile(filepath.Join(localDir, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(keepData))

	// Step 6: Re-sync should not mutate the converged test subtree, even if
	// other full-suite tests changed unrelated paths on the shared drive.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")
}

type directionalSyncMode struct {
	name       string
	flag       string
	stderrMode string
}

func directionalConflictModes() []directionalSyncMode {
	return []directionalSyncMode{
		{name: "download_only", flag: "--download-only", stderrMode: "Mode: download-only"},
		{name: "upload_only", flag: "--upload-only", stderrMode: "Mode: upload-only"},
	}
}

func runSyncWithMode(t *testing.T, cfgPath string, env map[string]string, mode directionalSyncMode) (string, string) {
	t.Helper()

	args := []string{"sync", mode.flag}
	return runCLIWithConfig(t, cfgPath, env, args...)
}

func requireSyncWithModeEventuallyConverges(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	mode directionalSyncMode,
	timeout time.Duration,
	description string,
	ready func(syncAttemptResult) bool,
) syncAttemptResult {
	t.Helper()
	return requireSyncEventuallyConverges(t, cfgPath, env, timeout, description, ready, mode.flag)
}

func conflictCopiesForPath(t *testing.T, absPath string) []string {
	t.Helper()

	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)
	stem, ext := syncengine.ConflictStemExt(name)
	matches, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%s.conflict-*%s", stem, ext)))
	require.NoError(t, err)
	return matches
}

// Validates: R-2.1.3
func TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-dlonly-%d", time.Now().UnixNano())

	// Step 1: Create a baseline with one file that remote will own and one
	// file whose later local-only edit should stay deferred.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "remote-owned.txt"), []byte("initial remote-owned"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-deferred.txt"), []byte("initial local-deferred"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "remote-owned.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 2: Diverge one file remotely and a different file locally.
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-owned.txt", "remote modification")
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-deferred.txt"), []byte("local deferred modification"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-new.txt"), []byte("local new file"), 0o600))

	// Step 3: Download-only should apply the remote-only change but leave the
	// local-only changes deferred rather than uploading them.
	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"download-only should apply remote-only drift without uploading local-only changes",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			remoteOwnedData, err := os.ReadFile(filepath.Join(localDir, "remote-owned.txt"))
			if err != nil || string(remoteOwnedData) != "remote modification" {
				return false
			}

			deferredData, err := os.ReadFile(filepath.Join(localDir, "local-deferred.txt"))
			if err != nil || string(deferredData) != "local deferred modification" {
				return false
			}

			lsOut, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder)
			return !strings.Contains(lsOut, "local-new.txt")
		},
		"--download-only",
	)
	assert.Contains(t, attempt.Stderr, "Mode: download-only")

	// Step 4: The deferred local edit still has its old remote content.
	assert.Equal(t, "initial local-deferred", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/local-deferred.txt"))

	// Step 5: A later upload-only pass should publish the deferred local-only changes.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "local-new.txt", pollTimeout, "ls", "/"+testFolder)
	assert.Equal(t, "local deferred modification", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/local-deferred.txt"))

	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.1.4
func TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-uponly-%d", time.Now().UnixNano())

	// Step 1: Create a local baseline that later diverges independently.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-owned.txt"), []byte("initial local-owned"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "remote-deferred.txt"), []byte("initial remote-deferred"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "remote-deferred.txt", pollTimeout, "ls", "/"+testFolder)

	// Step 2: Diverge one file locally and a different file remotely.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-owned.txt"), []byte("local modification"), 0o600))
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-deferred.txt", "remote deferred modification")
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-new.txt", "remote new file")

	// Step 3: Upload-only should publish the local-only edit without
	// overwriting or downloading the remote-only changes.
	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"upload-only should apply local-only drift without downloading remote-only changes",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			localDeferredData, err := os.ReadFile(filepath.Join(localDir, "remote-deferred.txt"))
			if err != nil || string(localDeferredData) != "initial remote-deferred" {
				return false
			}

			_, err = os.Stat(filepath.Join(localDir, "remote-new.txt"))
			return os.IsNotExist(err)
		},
		"--upload-only",
	)
	assert.Contains(t, attempt.Stderr, "Mode: upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "local-owned.txt", pollTimeout, "ls", "/"+testFolder)
	assert.Equal(t, "local modification", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/local-owned.txt"))
	assert.Equal(t, "remote deferred modification", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/remote-deferred.txt"))

	// Step 4: A later download-only pass should converge the deferred remote-only changes.
	downloadAttempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"download-only should later converge deferred remote-only changes",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			localDeferredData, err := os.ReadFile(filepath.Join(localDir, "remote-deferred.txt"))
			if err != nil || string(localDeferredData) != "remote deferred modification" {
				return false
			}

			newData, err := os.ReadFile(filepath.Join(localDir, "remote-new.txt"))
			if err != nil || string(newData) != "remote new file" {
				return false
			}

			return true
		},
		"--download-only",
	)
	assert.Contains(t, downloadAttempt.Stderr, "Mode: download-only")

	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.1.3, R-2.1.4, R-2.2, R-2.3.1
func TestE2E_Sync_DirectionalModes_PreserveEditEditConflict(t *testing.T) {
	registerLogDump(t)

	for _, mode := range directionalConflictModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			syncDir := t.TempDir()
			cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
			testFolder := fmt.Sprintf("e2e-sync-ee-%s-%d", mode.name, time.Now().UnixNano())

			localDir := filepath.Join(syncDir, testFolder)
			require.NoError(t, os.MkdirAll(localDir, 0o700))
			conflictFile := filepath.Join(localDir, "shared.txt")
			require.NoError(t, os.WriteFile(conflictFile, []byte("original v1"), 0o600))

			runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
			waitForRemoteReadContains(t, cfgPath, env, "", "shared.txt", pollTimeout, "ls", "/"+testFolder)

			require.NoError(t, os.WriteFile(conflictFile, []byte("local edit v2"), 0o600))
			putRemoteFile(t, cfgPath, env, "/"+testFolder+"/shared.txt", "remote edit v2")

			_, stderr := runSyncWithMode(t, cfgPath, env, mode)
			assert.Contains(t, stderr, mode.stderrMode)
			if mode.flag == "--upload-only" {
				// Upload-only defers the remote-winner download, so the paired
				// conflict-copy rename is suppressed too. Local truth must stay put.
				assert.NotContains(t, stderr, "Conflict copies:")
				assert.Contains(t, stderr, "Deferred by mode:")
				assert.Contains(t, stderr, "Downloads:")
				assert.Empty(t, conflictCopiesForPath(t, conflictFile))

				originalData, err := os.ReadFile(conflictFile)
				require.NoError(t, err)
				assert.Equal(t, "local edit v2", string(originalData))
				assert.Equal(t, "remote edit v2", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/shared.txt"))

				return
			}

			assert.Contains(t, stderr, "Conflict copies:")

			matches := conflictCopiesForPath(t, conflictFile)
			require.Len(t, matches, 1, "true directional conflicts must preserve the local version as a conflict copy")

			conflictCopyData, err := os.ReadFile(matches[0])
			require.NoError(t, err)
			assert.Equal(t, "local edit v2", string(conflictCopyData))

			originalData, err := os.ReadFile(conflictFile)
			require.NoError(t, err)
			assert.Equal(t, "remote edit v2", string(originalData))
		})
	}
}

// Validates: R-2.1.3, R-2.1.4, R-2.2
func TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict(t *testing.T) {
	registerLogDump(t)

	for _, mode := range directionalConflictModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			syncDir := t.TempDir()
			cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
			testFolder := fmt.Sprintf("e2e-sync-ed-%s-%d", mode.name, time.Now().UnixNano())

			localDir := filepath.Join(syncDir, testFolder)
			require.NoError(t, os.MkdirAll(localDir, 0o700))
			fragileFile := filepath.Join(localDir, "fragile.txt")
			require.NoError(t, os.WriteFile(fragileFile, []byte("precious data"), 0o600))

			runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
			runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
			waitForRemoteReadContains(t, cfgPath, env, "", "fragile.txt", pollTimeout, "ls", "/"+testFolder)

			require.NoError(t, os.WriteFile(fragileFile, []byte("locally modified precious data"), 0o600))
			runCLIWithConfig(t, cfgPath, env, "rm", "/"+testFolder+"/fragile.txt")
			waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "fragile.txt", "ls", "/"+testFolder)

			attempt := requireSyncWithModeEventuallyConverges(
				t,
				cfgPath,
				env,
				mode,
				120*time.Second,
				"directional mode should preserve local content across edit-delete conflict",
				func(result syncAttemptResult) bool {
					if result.Err != nil {
						return false
					}

					data, err := os.ReadFile(fragileFile)
					if err != nil {
						return false
					}

					if string(data) != "locally modified precious data" {
						return false
					}

					stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, "stat", "/"+testFolder+"/fragile.txt")
					if mode.flag == "--download-only" {
						if err == nil {
							t.Logf("waiting for remote delete to stay visible via stat disappearance\nstdout: %s\nstderr: %s", stdout, stderr)
							return false
						}

						return true
					}

					if err != nil {
						t.Logf("waiting for recreated remote file\nstdout: %s\nstderr: %s", stdout, stderr)
						return false
					}

					return getRemoteFile(t, cfgPath, env, "/"+testFolder+"/fragile.txt") == "locally modified precious data"
				},
			)
			assert.Contains(t, attempt.Stderr, mode.stderrMode)

			data, err := os.ReadFile(fragileFile)
			require.NoError(t, err)
			assert.Equal(t, "locally modified precious data", string(data))

			if mode.flag == "--download-only" {
				assert.Contains(t, attempt.Stderr, "Deferred by mode:")
				assert.Contains(t, attempt.Stderr, "Uploads:")
				waitForRemoteDeleteDisappearance(t, cfgPath, env, resolveDriveSelection(env, ""), "fragile.txt", "ls", "/"+testFolder)
				assert.Empty(t, conflictCopiesForPath(t, fragileFile), "directional edit-delete should preserve the local canonical file without a conflict copy")
				return
			}

			waitForRemoteReadContains(t, cfgPath, env, "", "fragile.txt", pollTimeout, "ls", "/"+testFolder)
			assert.Equal(t, "locally modified precious data", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/fragile.txt"))
			assert.Empty(t, conflictCopiesForPath(t, fragileFile), "edit-delete recreate upload should not create a conflict copy")
		})
	}
}

// Validates: R-2.1.3, R-2.1.4, R-2.2, R-2.3.1
func TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict(t *testing.T) {
	registerLogDump(t)

	for _, mode := range directionalConflictModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			syncDir := t.TempDir()
			cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
			testFolder := fmt.Sprintf("e2e-sync-cc-%s-%d", mode.name, time.Now().UnixNano())

			localDir := filepath.Join(syncDir, testFolder)
			require.NoError(t, os.MkdirAll(localDir, 0o700))
			conflictFile := filepath.Join(localDir, "collision.txt")
			require.NoError(t, os.WriteFile(conflictFile, []byte("local version"), 0o600))

			mkdirRemoteFolder(t, cfgPath, env, "/"+testFolder)
			putRemoteFile(t, cfgPath, env, "/"+testFolder+"/collision.txt", "remote version")

			_, stderr := runSyncWithMode(t, cfgPath, env, mode)
			assert.Contains(t, stderr, mode.stderrMode)
			if mode.flag == "--upload-only" {
				// Upload-only defers the remote-winner download, so the local
				// canonical file must remain untouched instead of being renamed.
				assert.NotContains(t, stderr, "Conflict copies:")
				assert.Contains(t, stderr, "Deferred by mode:")
				assert.Contains(t, stderr, "Downloads:")
				assert.Empty(t, conflictCopiesForPath(t, conflictFile))

				originalData, err := os.ReadFile(conflictFile)
				require.NoError(t, err)
				assert.Equal(t, "local version", string(originalData))
				assert.Equal(t, "remote version", getRemoteFile(t, cfgPath, env, "/"+testFolder+"/collision.txt"))

				return
			}

			assert.Contains(t, stderr, "Conflict copies:")

			matches := conflictCopiesForPath(t, conflictFile)
			require.Len(t, matches, 1, "create-create conflicts must preserve the local version instead of overwriting it")

			conflictCopyData, err := os.ReadFile(matches[0])
			require.NoError(t, err)
			assert.Equal(t, "local version", string(conflictCopyData))

			originalData, err := os.ReadFile(conflictFile)
			require.NoError(t, err)
			assert.Equal(t, "remote version", string(originalData))
		})
	}
}

// TestE2E_Sync_NestedFolderHierarchy exercises deep folder creation, file
// operations at multiple depths, and verify across the hierarchy.
func TestE2E_Sync_NestedFolderHierarchy(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-nested-%d", time.Now().UnixNano())

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
	waitForRemoteReadContains(t, cfgPath, env, "", "deep.txt", pollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Step 3: Set up mixed changes.
	// New remote deeper folder.
	mkdirRemoteFolder(t, cfgPath, env, "/"+testFolder+"/a/b/c/d")
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/a/b/c/d/deeper.txt", "deeper content")

	// Delete local sibling.
	require.NoError(t, os.Remove(filepath.Join(localDir, "a", "sibling.txt")))

	// New local deeper folder.
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "x", "y", "z"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "z", "new-leaf.txt"), []byte("leaf content"), 0o600))

	// Modify local file.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "x", "y", "another.txt"), []byte("modified another"), 0o600))

	// Step 4: Bidirectional sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 5: Assert results.
	// Remote deeper.txt downloaded locally.
	deeperData, err := os.ReadFile(filepath.Join(localDir, "a", "b", "c", "d", "deeper.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deeper content", string(deeperData))

	// sibling.txt removed remotely.
	siblingOut, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder+"/a")
	assert.NotContains(t, siblingOut, "sibling.txt")

	// new-leaf.txt uploaded remotely.
	leafOut, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder+"/x/y/z")
	assert.Contains(t, leafOut, "new-leaf.txt")

	// another.txt uploaded with modification.
	remoteAnother := getRemoteFile(t, cfgPath, env, "/"+testFolder+"/x/y/another.txt")
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
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-dryrun-%d", time.Now().UnixNano())

	// Step 1: Create and sync an initial file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("existing content"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Step 2: Set up pending changes.
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "pending-upload.txt"), []byte("to upload"), 0o600))
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/pending-download.txt", "to download")
	require.NoError(t, os.Remove(filepath.Join(localDir, "existing.txt")))

	// Step 3: Dry-run sync.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--dry-run")
	assert.Contains(t, stderr, "Dry run")

	// Step 4: Verify NO side effects.
	// pending-upload.txt NOT remotely uploaded.
	lsOut, _ := waitForRemoteReadContains(t, cfgPath, env, "", "existing.txt", pollTimeout, "ls", "/"+testFolder)
	assert.NotContains(t, lsOut, "pending-upload.txt", "dry-run should not upload")

	// pending-download.txt NOT locally downloaded.
	_, err := os.Stat(filepath.Join(localDir, "pending-download.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run should not download")

	// existing.txt still present remotely (not deleted).
	assert.Contains(t, lsOut, "existing.txt", "dry-run should not delete remotely")

	// Step 5: Real sync.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Step 6: All changes applied (poll for eventual consistency).
	lsOut, _ = waitForRemoteReadContains(t, cfgPath, env, "", "pending-upload.txt", pollTimeout, "ls", "/"+testFolder)
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "existing.txt", "ls", "/"+testFolder)
	assert.Contains(t, lsOut, "pending-upload.txt")

	dlData, err := os.ReadFile(filepath.Join(localDir, "pending-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "to download", string(dlData))

	// Step 7: Internal baseline verification stays clean.
	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
}

// TestE2E_Sync_ConvergentEdit exercises same-content edit and same-content
// create convergence. The owned subtree should remain converged without
// conflict copies even if unrelated full-suite remote churn causes transfers
// elsewhere on the shared drive.
func TestE2E_Sync_ConvergentEdit(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-conv-%d", time.Now().UnixNano())

	// Step 1: Create and sync baseline file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Advance delta token past the upload so the subsequent bidirectional
	// sync uses incremental delta, not full enumeration (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Step 2: Modify both sides to the same content.
	newContent := "convergent new content"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-edit.txt"), []byte(newContent), 0o600))
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/converge-edit.txt", newContent)

	// Step 3: Create a new file on both sides with the same content.
	freshContent := "fresh convergent"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "converge-create.txt"), []byte(freshContent), 0o600))
	putRemoteFile(t, cfgPath, env, "/"+testFolder+"/converge-create.txt", freshContent)

	// Step 4: Bidirectional sync — convergent detection.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")

	// Convergent updates detected (no data transfer needed).
	assert.Contains(t, stderr, "Baseline updates:")
	assert.NotContains(t, stderr, "Conflict copies:")

	editData, err := os.ReadFile(filepath.Join(localDir, "converge-edit.txt"))
	require.NoError(t, err)
	assert.Equal(t, newContent, string(editData))

	createData, err := os.ReadFile(filepath.Join(localDir, "converge-create.txt"))
	require.NoError(t, err)
	assert.Equal(t, freshContent, string(createData))

	assert.Equal(t, newContent, getRemoteFile(t, cfgPath, env, "/"+testFolder+"/converge-edit.txt"))
	assert.Equal(t, freshContent, getRemoteFile(t, cfgPath, env, "/"+testFolder+"/converge-create.txt"))

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
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	testFolder := fmt.Sprintf("e2e-sync-tamp-%d", time.Now().UnixNano())

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
