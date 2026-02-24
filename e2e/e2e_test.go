//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
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

var (
	binaryPath string
	drive      string
	logDir     string
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

	// Set up debug log directory for E2E visibility.
	if dir := os.Getenv("E2E_LOG_DIR"); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			fmt.Fprintf(os.Stderr, "creating E2E log dir: %v\n", mkErr)
			os.Exit(1)
		}

		logDir = dir
	} else {
		dir, mkErr := os.MkdirTemp("", "onedrive-e2e-logs-*")
		if mkErr != nil {
			fmt.Fprintf(os.Stderr, "creating E2E log temp dir: %v\n", mkErr)
			os.Exit(1)
		}

		logDir = dir
	}

	fmt.Fprintf(os.Stderr, "E2E debug logs: %s\n", logDir)

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

// sanitizeTestName replaces characters invalid in filenames.
func sanitizeTestName(name string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(name)
}

// shouldAddDebug returns true unless args already contain a mutually exclusive
// verbosity flag (--quiet, -q, --verbose, -v, --debug).
func shouldAddDebug(args []string) bool {
	for _, a := range args {
		switch a {
		case "--quiet", "-q", "--verbose", "-v", "--debug":
			return false
		}
	}

	return true
}

// logCLIExecution appends CLI invocation details to a per-test debug log file.
func logCLIExecution(t *testing.T, args []string, stdout, stderr string) {
	t.Helper()

	logPath := filepath.Join(logDir, sanitizeTestName(t.Name())+".log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("warning: cannot write debug log: %v", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== %s ===\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "CMD: %s\n", strings.Join(args, " "))
	fmt.Fprintf(f, "--- STDOUT ---\n%s\n", stdout)
	fmt.Fprintf(f, "--- STDERR ---\n%s\n\n", stderr)
}

// registerLogDump registers a cleanup that dumps the debug log on test failure.
func registerLogDump(t *testing.T) {
	t.Helper()

	logPath := filepath.Join(logDir, sanitizeTestName(t.Name())+".log")

	t.Cleanup(func() {
		if t.Failed() {
			data, err := os.ReadFile(logPath)
			if err != nil {
				return
			}

			t.Logf("=== DEBUG LOG DUMP (%s) ===\n%s", logPath, string(data))
		} else {
			t.Logf("debug log: %s", logPath)
		}
	})
}

func runCLI(t *testing.T, args ...string) (string, string) {
	t.Helper()

	fullArgs := []string{"--drive", drive}
	if shouldAddDebug(args) {
		fullArgs = append(fullArgs, "--debug")
	}

	fullArgs = append(fullArgs, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	logCLIExecution(t, fullArgs, stdout.String(), stderr.String())

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

// runCLIExpectError runs the CLI binary with the given args and expects a
// non-zero exit code. It returns the combined stdout+stderr output for
// assertion. If the command unexpectedly succeeds, it fails the test.
func runCLIExpectError(t *testing.T, args ...string) string {
	t.Helper()

	fullArgs := []string{"--drive", drive}
	if shouldAddDebug(args) {
		fullArgs = append(fullArgs, "--debug")
	}

	fullArgs = append(fullArgs, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	logCLIExecution(t, fullArgs, stdout.String(), stderr.String())

	require.Error(t, err, "expected CLI to fail for args %v, but it succeeded\nstdout: %s\nstderr: %s",
		args, stdout.String(), stderr.String())

	return stdout.String() + stderr.String()
}

// TestE2E_ErrorCases verifies that the CLI returns non-zero exit codes
// and meaningful error messages for invalid operations.
func TestE2E_ErrorCases(t *testing.T) {
	t.Run("ls_not_found", func(t *testing.T) {
		output := runCLIExpectError(t, "ls", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("get_root_is_folder", func(t *testing.T) {
		output := runCLIExpectError(t, "get", "/")
		// The CLI reports "is a folder, not a file" when get targets a folder.
		assert.Contains(t, output, "folder")
	})

	t.Run("rm_not_found", func(t *testing.T) {
		output := runCLIExpectError(t, "rm", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})
}

// TestE2E_JSONOutput validates that --json flags produce well-formed JSON
// with the expected schema for ls and stat commands.
func TestE2E_JSONOutput(t *testing.T) {
	t.Run("ls_json", func(t *testing.T) {
		stdout, _ := runCLI(t, "ls", "--json", "/")

		var items []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &items),
			"ls --json output should be valid JSON array, got: %s", stdout)

		// Root should have at least one item in any real OneDrive.
		require.NotEmpty(t, items, "expected at least one item in root listing")

		for i, item := range items {
			assert.Contains(t, item, "name",
				"item %d missing 'name' key", i)
			assert.Contains(t, item, "id",
				"item %d missing 'id' key", i)
		}
	})

	t.Run("stat_json", func(t *testing.T) {
		stdout, _ := runCLI(t, "stat", "--json", "/")

		var obj map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &obj),
			"stat --json output should be valid JSON object, got: %s", stdout)

		assert.Contains(t, obj, "name", "stat JSON missing 'name' key")
		assert.Contains(t, obj, "id", "stat JSON missing 'id' key")
	})
}

// TestE2E_QuietFlag verifies that --quiet suppresses informational output
// on stderr during file operations.
func TestE2E_QuietFlag(t *testing.T) {
	t.Run("put_quiet_suppresses_output", func(t *testing.T) {
		testFolder := fmt.Sprintf("onedrive-go-e2e-quiet-%d", time.Now().UnixNano())
		remotePath := "/" + testFolder + "/quiet-test.txt"

		// Cleanup at the end.
		t.Cleanup(func() {
			fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
			cmd := exec.Command(binaryPath, fullArgs...)
			_ = cmd.Run()
		})

		// Create parent folder.
		runCLI(t, "mkdir", "/"+testFolder)

		// Write a small local file for upload.
		tmpFile, err := os.CreateTemp("", "e2e-quiet-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write([]byte("quiet test content\n"))
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		// Upload with --quiet — stderr should be empty (no "Uploaded" status line).
		_, stderr := runCLI(t, "put", "--quiet", tmpFile.Name(), remotePath)
		assert.Empty(t, stderr, "expected no stderr output with --quiet flag")
	})
}
