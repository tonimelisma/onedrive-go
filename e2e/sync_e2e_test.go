//go:build e2e

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

// writeSyncConfig creates a minimal TOML config file pointing to the given
// syncDir for the test drive. Returns the path to the temp config file.
func writeSyncConfig(t *testing.T, syncDir string) string {
	t.Helper()

	// The drive variable comes from TestMain (e.g., "personal:testitesti18@outlook.com").
	// The config format is: ["personal:testitesti18@outlook.com"]\nsync_dir = "/path"
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

	fullArgs := append([]string{"--config", cfgPath, "--drive", drive}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
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

	fullArgs := append([]string{"--config", cfgPath, "--drive", drive}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

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
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Verify file appeared remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
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
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --download-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--download-only")
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
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Run sync --dry-run --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--dry-run")
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
		fullArgs := []string{"--drive", drive, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	// Sync to establish baseline.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only")

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
