//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	binaryPath string
	drive      string
)

func TestMain(m *testing.M) {
	// Build binary to temp dir.
	tmpDir, err := os.MkdirTemp("", "onedrive-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	binaryPath = filepath.Join(tmpDir, "onedrive-go")

	cmd := exec.Command("go", "build", "-o", binaryPath, ".")
	cmd.Dir = findModuleRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building binary: %v\n", err)
		os.Exit(1)
	}

	drive = os.Getenv("ONEDRIVE_TEST_DRIVE")
	if drive == "" {
		drive = "personal:test@example.com"
	}

	os.Exit(m.Run())
}

// findModuleRoot walks up from the current dir to find go.mod.
func findModuleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Fallback to ".." — e2e/ is one level below module root.
			return ".."
		}

		dir = parent
	}
}

func runCLI(t *testing.T, args ...string) (string, string) {
	t.Helper()

	fullArgs := append([]string{"--drive", drive}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("CLI command %v failed: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String()
}

func TestE2E_RoundTrip(t *testing.T) {
	testFolder := fmt.Sprintf("onedrive-go-e2e-%d", time.Now().UnixNano())
	testSubfolder := testFolder + "/subfolder"
	testFile := testFolder + "/test.txt"
	testContent := []byte("Hello from onedrive-go E2E test!\n")

	// Cleanup at the end — delete the test folder.
	t.Cleanup(func() {
		// Best-effort cleanup — ignore errors.
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	t.Run("whoami", func(t *testing.T) {
		stdout, _ := runCLI(t, "whoami", "--json")

		var out map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out))
		assert.Contains(t, out, "user")
		assert.Contains(t, out, "drives")

		drives, ok := out["drives"].([]interface{})
		require.True(t, ok)
		assert.NotEmpty(t, drives)
	})

	t.Run("ls_root", func(t *testing.T) {
		stdout, _ := runCLI(t, "ls", "/")
		assert.Contains(t, stdout, "NAME")
	})

	t.Run("mkdir", func(t *testing.T) {
		_, stderr := runCLI(t, "mkdir", "/"+testSubfolder)
		assert.Contains(t, stderr, "Created")
	})

	t.Run("put", func(t *testing.T) {
		// Write test content to a local temp file.
		tmpFile, err := os.CreateTemp("", "e2e-upload-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write(testContent)
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLI(t, "put", tmpFile.Name(), "/"+testFile)
		assert.Contains(t, stderr, "Uploaded")
	})

	t.Run("ls_folder", func(t *testing.T) {
		stdout, _ := runCLI(t, "ls", "/"+testFolder)
		assert.Contains(t, stdout, "test.txt")
		assert.Contains(t, stdout, "subfolder")
	})

	t.Run("stat", func(t *testing.T) {
		stdout, _ := runCLI(t, "stat", "/"+testFile)
		assert.Contains(t, stdout, "test.txt")
		assert.Contains(t, stdout, fmt.Sprintf("%d bytes", len(testContent)))
	})

	t.Run("get", func(t *testing.T) {
		tmpDir := t.TempDir()
		localPath := filepath.Join(tmpDir, "downloaded.txt")

		_, stderr := runCLI(t, "get", "/"+testFile, localPath)
		assert.Contains(t, stderr, "Downloaded")

		downloaded, err := os.ReadFile(localPath)
		require.NoError(t, err)
		assert.Equal(t, testContent, downloaded)
	})

	t.Run("rm_file", func(t *testing.T) {
		_, stderr := runCLI(t, "rm", "/"+testFile)
		assert.Contains(t, stderr, "Deleted")
	})

	t.Run("rm_subfolder", func(t *testing.T) {
		_, stderr := runCLI(t, "rm", "/"+testSubfolder)
		assert.Contains(t, stderr, "Deleted")
	})
}

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
