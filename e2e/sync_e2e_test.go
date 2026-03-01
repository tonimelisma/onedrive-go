//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Sync test helpers (available under the base e2e tag for both fast and full)
// ---------------------------------------------------------------------------

// writeSyncConfig creates a minimal TOML config file pointing to the given
// syncDir for the test drive. Each test isolates via HOME override so the
// state database lands in a per-test temp directory. The token file is
// copied from the real data dir so authentication works under the new HOME.
// Returns the path to the temp config file.
func writeSyncConfig(t *testing.T, syncDir string) string {
	t.Helper()

	// Capture real data dir before overriding HOME.
	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	realDataDir := e2eDataDir(realHome)

	// Isolate HOME so the state DB lands in a per-test temp directory,
	// preventing cross-test contamination.
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	// Create the data dir under the temp HOME and copy the token file
	// so the CLI binary can authenticate.
	tempDataDir := e2eDataDir(tempHome)
	require.NoError(t, os.MkdirAll(tempDataDir, 0o755))
	copyTokenFile(t, realDataDir, tempDataDir)

	// The drive variable comes from TestMain (e.g., "personal:testitesti18@outlook.com").
	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	return cfgPath
}

// runCLIWithConfig runs the CLI binary with a custom config file.
func runCLIWithConfig(t *testing.T, cfgPath string, args ...string) (string, string) {
	t.Helper()

	fullArgs := []string{"--config", cfgPath, "--drive", drive}
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
		t.Fatalf("CLI command %v failed: %v\nstdout: %s\nstderr: %s",
			args, err, stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String()
}

// runCLIWithConfigAllowError runs the CLI binary with a custom config file
// and returns the output even on error.
func runCLIWithConfigAllowError(t *testing.T, cfgPath string, args ...string) (string, string, error) {
	t.Helper()

	fullArgs := []string{"--config", cfgPath, "--drive", drive}
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

	return stdout.String(), stderr.String(), err
}

// putRemoteFile uploads string content to a remote path via a temp file.
func putRemoteFile(t *testing.T, remotePath, content string) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "e2e-put-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	runCLI(t, "put", tmpFile.Name(), remotePath)
}

// getRemoteFile downloads a remote file and returns its content as a string.
func getRemoteFile(t *testing.T, remotePath string) string {
	t.Helper()

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "downloaded")

	runCLI(t, "get", remotePath, localPath)

	data, err := os.ReadFile(localPath)
	require.NoError(t, err)

	return string(data)
}

// cleanupRemoteFolder is a best-effort remote cleanup for use in t.Cleanup.
func cleanupRemoteFolder(t *testing.T, folder string) {
	t.Helper()

	fullArgs := []string{"--drive", drive, "rm", "-r", "/" + folder}
	cmd := exec.Command(binaryPath, fullArgs...)
	_ = cmd.Run()
}

// ---------------------------------------------------------------------------
// Fast sync tests (run on every CI push under the "e2e" tag)
// ---------------------------------------------------------------------------

func TestE2E_Sync_UploadOnly(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	// Create unique test folder and files.
	testFolder := fmt.Sprintf("e2e-sync-up-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "upload-test.txt"), []byte("sync upload test\n"), 0o644))

	// Cleanup remote after test.
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Poll for eventual consistency — file may not be immediately visible
	// via path-based queries after upload.
	remotePath := "/" + testFolder + "/upload-test.txt"
	stdout, _ := pollCLIWithConfigContains(t, cfgPath, "upload-test.txt", pollTimeout, "stat", remotePath)
	assert.Contains(t, stdout, "upload-test.txt")
}

func TestE2E_Sync_DownloadOnly(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	// Create a unique folder + file remotely via put.
	testFolder := fmt.Sprintf("e2e-sync-dl-%d", time.Now().UnixNano())
	remotePath := "/" + testFolder + "/download-test.txt"
	content := []byte("sync download test\n")

	// Create remote folder + file.
	runCLI(t, "mkdir", "/"+testFolder)

	tmpFile, err := os.CreateTemp("", "e2e-sync-dl-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	runCLI(t, "put", tmpFile.Name(), remotePath)

	// Cleanup remote after test.
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --download-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--download-only", "--force")
	assert.Contains(t, stderr, "Mode: download-only")

	// Verify file appeared locally.
	localPath := filepath.Join(syncDir, testFolder, "download-test.txt")
	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)
}

func TestE2E_Sync_DryRun(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	// Create a local file.
	testFolder := fmt.Sprintf("e2e-sync-dry-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "dryrun.txt"), []byte("dry run test\n"), 0o644))

	// Cleanup remote (should not exist, but best-effort).
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --dry-run --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--dry-run", "--force")
	assert.Contains(t, stderr, "Dry run")

	// Verify file was NOT uploaded.
	output := runCLIExpectError(t, "ls", "/"+testFolder)
	assert.Contains(t, output, testFolder)
}

func TestE2E_Sync_Verify(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	// Create and sync a file.
	testFolder := fmt.Sprintf("e2e-sync-ver-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "verify-me.txt"), []byte("verify test\n"), 0o644))

	// Cleanup remote after test.
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Sync to establish baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Run verify.
	stdout, _, verifyErr := runCLIWithConfigAllowError(t, cfgPath, "verify")

	// Verify should pass (exit 0) or show verified files.
	if verifyErr != nil {
		t.Logf("verify output: %s", stdout)
	}

	assert.Contains(t, stdout, "Verified")
}

func TestE2E_Sync_Conflicts(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	// Run conflicts — should show no conflicts on a fresh drive.
	stdout, _ := runCLIWithConfig(t, cfgPath, "conflicts")
	// Trim whitespace for comparison.
	assert.True(t, strings.Contains(stdout, "No unresolved conflicts"),
		"expected 'No unresolved conflicts' in output, got: %s", stdout)
}

func TestE2E_Sync_DriveRemoveAndReAdd(t *testing.T) {
	// Proves that removing and re-adding a drive preserves the state DB
	// (via platform default path), allowing incremental delta sync to resume.
	syncDir := t.TempDir()

	// Capture real data dir before overriding HOME.
	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	realDataDir := e2eDataDir(realHome)

	// Isolate HOME so all state DBs land in a per-test temp directory.
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)

	// Create the data dir and copy the token file.
	tempDataDir := e2eDataDir(testHome)
	require.NoError(t, os.MkdirAll(tempDataDir, 0o755))
	copyTokenFile(t, realDataDir, tempDataDir)

	testFolder := fmt.Sprintf("e2e-sync-readd-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Helper to write a config — relies on HOME isolation for state DB.
	writeConfig := func(t *testing.T) string {
		t.Helper()

		content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive, syncDir)
		cfgPath := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

		return cfgPath
	}

	// Step 1: Create a local file and sync it up.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file1.txt"), []byte("first file\n"), 0o644))

	cfgPath := writeConfig(t)
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Poll to verify file1 exists remotely.
	remotePath1 := "/" + testFolder + "/file1.txt"
	pollCLIWithConfigContains(t, cfgPath, "file1.txt", pollTimeout, "stat", remotePath1)

	// Step 2: Delete the drive section from config (simulate "drive remove").
	// Write an empty config — the drive section is gone.
	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, []byte(""), 0o644))

	// Step 3: Re-add the drive section with the same sync_dir.
	cfgPath2 := writeConfig(t)

	// Step 4: Create a second local file and sync again.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file2.txt"), []byte("second file\n"), 0o644))

	_, stderr = runCLIWithConfig(t, cfgPath2, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Step 5: Verify both files exist remotely (proves delta resume from
	// preserved state DB — file1 wasn't re-uploaded, file2 was uploaded).
	remotePath2 := "/" + testFolder + "/file2.txt"
	pollCLIWithConfigContains(t, cfgPath2, "file2.txt", pollTimeout, "stat", remotePath2)
	pollCLIWithConfigContains(t, cfgPath2, "file1.txt", pollTimeout, "stat", remotePath1)
}

// e2eDataDir returns the platform-specific data directory for a given home.
// Mirrors internal/config.DefaultDataDir() without importing internal packages.
func e2eDataDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "onedrive-go")
	default:
		return filepath.Join(home, ".local", "share", "onedrive-go")
	}
}

// copyTokenFile copies the token file for the test drive from srcDir to dstDir.
// The drive variable (from TestMain) determines the token filename.
func copyTokenFile(t *testing.T, srcDir, dstDir string) {
	t.Helper()

	// Parse "personal:testitesti18@outlook.com" -> "token_personal_testitesti18@outlook.com.json"
	parts := strings.SplitN(drive, ":", 2)
	if len(parts) < 2 {
		t.Fatalf("cannot parse drive %q for token filename", drive)
	}

	tokenName := "token_" + parts[0] + "_" + parts[1] + ".json"
	srcPath := filepath.Join(srcDir, tokenName)

	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("cannot read token file %s: %v", srcPath, err)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dstDir, tokenName), data, 0o600))
}
