//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// ---------------------------------------------------------------------------
// Sync test helpers (available under the base e2e tag for both fast and full)
// ---------------------------------------------------------------------------

// writeSyncConfig creates a minimal TOML config file pointing to the given
// syncDir for the test drive. Each test gets per-test state DB isolation via
// XDG_DATA_HOME override. The token file is copied from TestMain's isolated
// data dir (testDataDir). Returns the config path and environment overrides
// that must be passed to CLI child processes (not set in process env).
func writeSyncConfig(t *testing.T, syncDir string) (string, map[string]string) {
	t.Helper()

	// Per-test isolation: each test gets its own XDG_DATA_HOME and HOME so
	// state DBs don't collide. Env overrides are returned (not set via
	// t.Setenv) and passed explicitly to child processes via cmd.Env.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

// runCLICore is the shared implementation for all config-aware CLI runner
// helpers. It builds the argument list (optionally adding --config, --drive,
// and --debug), executes the binary, logs output, and returns stdout, stderr,
// and the execution error. driveID="" omits --drive (all-drives mode).
func runCLICore(t *testing.T, cfgPath string, env map[string]string, driveID string, args ...string) (string, string, error) {
	t.Helper()

	var fullArgs []string
	if cfgPath != "" {
		fullArgs = append(fullArgs, "--config", cfgPath)
	}

	if driveID != "" {
		fullArgs = append(fullArgs, "--drive", driveID)
	}

	if shouldAddDebug(args) {
		fullArgs = append(fullArgs, "--debug")
	}

	fullArgs = append(fullArgs, args...)
	cmd := makeCmd(fullArgs, env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	logCLIExecution(t, fullArgs, stdout.String(), stderr.String())

	return stdout.String(), stderr.String(), err
}

// runCLIWithConfig runs the CLI binary with a custom config file.
// env overrides (if non-nil) are applied to the child process environment.
func runCLIWithConfig(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, drive, args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigAllowError runs the CLI binary with a custom config file
// and returns the output even on error.
func runCLIWithConfigAllowError(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()

	return runCLICore(t, cfgPath, env, drive, args...)
}

// snapshotLocalTree captures a deterministic view of a test-owned local
// subtree. Full-suite sync tests share one live drive account, so unrelated
// remote churn can legitimately produce global delta activity between two sync
// passes. Comparing the owned subtree before and after a follow-up sync lets
// tests assert their own convergence without depending on the rest of the
// shared drive staying perfectly still.
func snapshotLocalTree(t *testing.T, root string) map[string]string {
	t.Helper()

	_, err := os.Stat(root)
	if os.IsNotExist(err) {
		return map[string]string{
			".": "missing",
		}
	}
	require.NoError(t, err)

	snapshot := map[string]string{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		key := filepath.ToSlash(rel)
		if d.IsDir() {
			snapshot[key] = "dir"
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		sum := sha256.Sum256(data)
		snapshot[key] = fmt.Sprintf(
			"file:%o:%d:%x",
			info.Mode().Perm(),
			len(data),
			sum,
		)

		return nil
	})
	require.NoError(t, err)

	return snapshot
}

// assertSyncLeavesLocalTreeStable proves that a follow-up sync did not mutate
// the caller-owned subtree, even if unrelated live-drive activity caused other
// delta events elsewhere in the shared test account.
func assertSyncLeavesLocalTreeStable(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	root string,
	args ...string,
) string {
	t.Helper()

	before := snapshotLocalTree(t, root)
	_, stderr := runCLIWithConfig(t, cfgPath, env, args...)
	after := snapshotLocalTree(t, root)
	assert.Equal(t, before, after, "sync should not mutate the test-owned local tree")

	return stderr
}

// putRemoteFile uploads string content to a remote path via a temp file.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process. The helper deliberately
// relies on the CLI put command's own parent-path convergence boundary instead
// of proving parent visibility in a separate preflight command, because Graph
// can make a fresh folder visible to one command before the next path read
// stabilizes.
func putRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath, content string) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "e2e-put-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	runCLIWithConfig(t, cfgPath, env, "put", tmpFile.Name(), remotePath)
	pollRemotePathVisible(t, cfgPath, env, remotePath)
}

// getRemoteFile downloads a remote file and returns its content as a string.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process.
func getRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath string) string {
	t.Helper()

	pollRemotePathVisible(t, cfgPath, env, remotePath)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "downloaded")

	runCLIWithConfig(t, cfgPath, env, "get", remotePath, localPath)

	data, err := os.ReadFile(localPath)
	require.NoError(t, err)

	return string(data)
}

func pollRemoteParentVisible(t *testing.T, cfgPath string, env map[string]string, remotePath string) {
	t.Helper()

	cleanPath := path.Clean(remotePath)
	parent := path.Dir(cleanPath)
	if parent == "." || parent == "/" || parent == "" {
		return
	}

	pollRemotePathVisible(t, cfgPath, env, parent)
}

func pollRemotePathVisible(t *testing.T, cfgPath string, env map[string]string, remotePath string) {
	t.Helper()

	cleanPath := path.Clean(remotePath)
	if cleanPath == "." || cleanPath == "/" || cleanPath == "" {
		return
	}

	base := path.Base(cleanPath)
	if base == "." || base == "/" || base == "" {
		return
	}

	parent := path.Dir(cleanPath)
	if parent == "." || parent == "" {
		parent = "/"
	}

	waitForRemotePathVisible(t, cfgPath, env, cleanPath, parent, base)
}

func waitForRemotePathVisible(t *testing.T, cfgPath string, env map[string]string, cleanPath string, parent string, base string) {
	t.Helper()

	deadline := time.Now().Add(remoteWritePropagationTimeout)
	startedAt := time.Now()

	var lastStdout string
	var lastStderr string

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, "stat", cleanPath)
		if err == nil && strings.Contains(stdout, base) {
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote path visibility for %q", cleanPath),
				drive,
				[]string{"stat", cleanPath},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return
		}

		lastStdout = stdout
		lastStderr = stderr

		stdout, stderr, err = runCLIWithConfigAllowError(t, cfgPath, env, "ls", parent)
		if err == nil && strings.Contains(stdout, base) {
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote path visibility for %q", cleanPath),
				drive,
				[]string{"ls", parent},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return
		}

		lastStdout = stdout
		lastStderr = stderr

		if time.Now().After(deadline) {
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote path visibility for %q", cleanPath),
				drive,
				[]string{"stat", cleanPath, "ls", parent},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeTimeout,
			)
			require.Failf(
				t,
				"waitForRemotePathVisible: timed out",
				"after %v waiting for %q via stat %q or ls %q\nlast stdout: %s\nlast stderr: %s",
				remoteWritePropagationTimeout,
				cleanPath,
				cleanPath,
				parent,
				lastStdout,
				lastStderr,
			)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// cleanupRemoteFolder is a best-effort remote cleanup for use in t.Cleanup.
func cleanupRemoteFolder(t *testing.T, folder string) {
	t.Helper()
	cleanupRemoteFolderForDrive(t, drive, folder)
}

// ---------------------------------------------------------------------------
// Fast sync tests (run on every CI push under the "e2e" tag)
// ---------------------------------------------------------------------------
//
// These tests intentionally run sequentially. Sync currently observes the
// whole drive, so concurrent remote mutations from sibling tests can pollute
// the delta feed and make fixture expectations nondeterministic.

func TestE2E_Sync_UploadOnly(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Create unique test folder and files.
	testFolder := fmt.Sprintf("e2e-sync-up-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "upload-test.txt"), []byte("sync upload test\n"), 0o600))

	// Cleanup remote after test.
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run sync --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Upload-only success is persisted in the durable baseline immediately even
	// when follow-on remote path reads still lag Graph visibility convergence.
	relPath := filepath.ToSlash(filepath.Join(testFolder, "upload-test.txt"))
	requireBaselinePathPresent(t, env, relPath)
}

func TestE2E_Sync_DownloadOnly(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	// Create a unique folder + file remotely via put.
	testFolder := fmt.Sprintf("e2e-sync-dl-%d", time.Now().UnixNano())
	remotePath := "/" + testFolder + "/download-test.txt"
	content := []byte("sync download test\n")

	// Create remote folder + file.
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)

	tmpFile, err := os.CreateTemp("", "e2e-sync-dl-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	runCLIWithConfig(t, opsCfgPath, nil, "put", tmpFile.Name(), remotePath)
	waitForRemoteWriteVisible(t, opsCfgPath, nil, drive, "download-test.txt", "stat", remotePath)

	// Cleanup remote after test.
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run sync --download-only.
	localPath := filepath.Join(syncDir, testFolder, "download-test.txt")
	var downloaded []byte
	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		90*time.Second,
		"download-only sync should eventually materialize the remote file after delta catches up",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			data, readErr := os.ReadFile(localPath)
			if readErr != nil {
				return false
			}

			downloaded = data
			return bytes.Equal(downloaded, content)
		},
		"--download-only",
		"--force",
	)
	assert.Contains(t, attempt.Stderr, "Mode: download-only")
	assert.Equal(t, content, downloaded)
}

func TestE2E_Sync_DryRun(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Create a local file.
	testFolder := fmt.Sprintf("e2e-sync-dry-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "dryrun.txt"), []byte("dry run test\n"), 0o600))

	// Cleanup remote (should not exist, but best-effort).
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Run sync --dry-run --upload-only.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--dry-run", "--force")
	assert.Contains(t, stderr, "Dry run")

	// Verify file was NOT uploaded.
	opsCfgPath := writeMinimalConfig(t)
	output := runCLIWithConfigExpectError(t, opsCfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, output, testFolder)
}

func TestE2E_Sync_Verify(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Create and sync a file.
	testFolder := fmt.Sprintf("e2e-sync-ver-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "verify-me.txt"), []byte("verify test\n"), 0o600))

	// Cleanup remote after test.
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Sync to establish baseline.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Run verify.
	stdout, _, verifyErr := runCLIWithConfigAllowError(t, cfgPath, env, "verify")

	// Verify should pass (exit 0) or show verified files.
	if verifyErr != nil {
		t.Logf("verify output: %s", stdout)
	}

	assert.Contains(t, stdout, "Verified")
}

func TestE2E_Sync_Conflicts(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Run conflicts — should show no conflicts on a fresh drive.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "conflicts")
	assert.True(t, strings.Contains(stdout, "No conflicts."),
		"expected 'No conflicts.' in output, got: %s", stdout)
}

func TestE2E_Sync_DriveRemoveAndReAdd(t *testing.T) {
	registerLogDump(t)

	// Proves that removing and re-adding a drive preserves the state DB
	// (via platform default path), allowing incremental delta sync to resume.
	syncDir := t.TempDir()

	// Per-test isolation: each child process gets its own XDG_DATA_HOME and
	// HOME via explicit cmd.Env, avoiding global env mutation.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	tempDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(tempDataDir, 0o700))
	copyTokenFile(t, testDataDir, tempDataDir)

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	testFolder := fmt.Sprintf("e2e-sync-readd-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Helper to write a config — relies on HOME isolation for state DB.
	writeConfig := func(t *testing.T) string {
		t.Helper()

		content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive, syncDir)
		cfgPath := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

		return cfgPath
	}

	// Step 1: Create a local file and sync it up.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file1.txt"), []byte("first file\n"), 0o644))

	cfgPath := writeConfig(t)
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// The durable baseline table is the authoritative proof for this test's
	// claim: removing and re-adding the drive must preserve sync state so the
	// next upload-only pass resumes from the existing baseline instead of
	// starting fresh. Remote path readability can legitimately lag success.
	firstRelPath := filepath.ToSlash(filepath.Join(testFolder, "file1.txt"))
	requireBaselinePathPresent(t, env, firstRelPath)

	// Step 2: Delete the drive section from config (simulate "drive remove").
	// Write an empty config — the drive section is gone.
	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, []byte(""), 0o600))

	// Step 3: Re-add the drive section with the same sync_dir.
	cfgPath2 := writeConfig(t)

	// Step 4: Create a second local file and sync again.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file2.txt"), []byte("second file\n"), 0o644))

	_, stderr = runCLIWithConfig(t, cfgPath2, env, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Step 5: Verify both baseline rows exist in the preserved state DB. That
	// proves the second run reused durable state from file1 and appended file2
	// under the same drive-owned store after the config section was removed and
	// re-added.
	secondRelPath := filepath.ToSlash(filepath.Join(testFolder, "file2.txt"))
	requireBaselinePathPresent(t, env, firstRelPath)
	requireBaselinePathPresent(t, env, secondRelPath)
}

// copyTokenFile copies the token file for the test drive from srcDir to dstDir.
// The drive variable (from TestMain) determines the token filename.
func copyTokenFile(t *testing.T, srcDir, dstDir string) {
	t.Helper()

	name := testutil.TokenFileName(drive)
	srcPath := filepath.Join(srcDir, name)

	data, err := os.ReadFile(srcPath)
	require.NoErrorf(t, err, "cannot read token file %s", srcPath)

	require.NoError(t, os.WriteFile(filepath.Join(dstDir, name), data, 0o600))

	// Copy account profile and drive metadata files.
	testutil.CopyMetadataFiles(srcDir, dstDir)
}

// ---------------------------------------------------------------------------
// Polling helpers for daemon watch tests
// ---------------------------------------------------------------------------

// pollLocalFileExists polls until the file at path exists on disk or timeout.
func pollLocalFileExists(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		if _, err := os.Stat(path); err == nil {
			return
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollLocalFileExists: timed out",
				"after %v waiting for %s to exist", timeout, path)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// pollLocalFileContent polls until the file at path has the expected content.
func pollLocalFileContent(t *testing.T, path, expected string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == expected {
			return
		}

		if time.Now().After(deadline) {
			var last string
			if err != nil {
				last = fmt.Sprintf("error: %v", err)
			} else {
				last = fmt.Sprintf("content: %q", string(data))
			}

			require.Failf(t, "pollLocalFileContent: timed out",
				"after %v waiting for %s to contain %q\nlast: %s",
				timeout, path, expected, last)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// pollLocalDirGone polls until the directory at path no longer exists.
func pollLocalDirGone(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollLocalDirGone: timed out",
				"after %v waiting for %s to be removed", timeout, path)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// ---------------------------------------------------------------------------
// Config helpers for extended test scenarios
// ---------------------------------------------------------------------------

// writeSyncConfigWithOptions creates a TOML config like writeSyncConfig but
// appends extra TOML key-value pairs before the drive section. The extraTOML
// string contains global-level config keys (e.g., "transfer_workers = 2\n").
func writeSyncConfigWithOptions(t *testing.T, syncDir string, extraTOML string) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("%s\n[%q]\nsync_dir = %q\n", extraTOML, drive, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

// writeSyncConfigNoDrive creates a config file with no drive sections.
// Used to test status output when no drives are configured.
func writeSyncConfigNoDrive(t *testing.T) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("# no drives configured\n"), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

// ---------------------------------------------------------------------------
// Multi-drive helpers (used by orchestrator_e2e_test.go)
// ---------------------------------------------------------------------------

// writeMultiDriveConfig creates a TOML config file with both test drives,
// each pointing to its own sync directory. Both token files are copied to
// the per-test data directory. Returns config path and environment overrides.
func writeMultiDriveConfig(t *testing.T, syncDir1, syncDir2 string) (string, map[string]string) {
	t.Helper()
	require.NotEmpty(t, drive2, "drive2 must be set for multi-drive tests")

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))

	// Copy both token files.
	copyTokenFile(t, testDataDir, perTestDataDir)
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, drive2)

	content := fmt.Sprintf("[%q]\nsync_dir = %q\n\n[%q]\nsync_dir = %q\n",
		drive, syncDir1, drive2, syncDir2)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

// copyTokenFileForDrive copies the token file for a specific drive ID.
func copyTokenFileForDrive(t *testing.T, srcDir, dstDir, driveID string) {
	t.Helper()

	name := testutil.TokenFileName(driveID)
	srcPath := filepath.Join(srcDir, name)

	data, err := os.ReadFile(srcPath)
	require.NoErrorf(t, err, "cannot read token file %s", srcPath)

	require.NoError(t, os.WriteFile(filepath.Join(dstDir, name), data, 0o600))

	// Copy account profile and drive metadata files.
	testutil.CopyMetadataFiles(srcDir, dstDir)
}

// runCLIWithConfigAllDrives runs the CLI without --drive flag (syncs all drives).
func runCLIWithConfigAllDrives(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigAllDrivesAllowError runs the CLI without --drive flag and
// returns the output even on error.
func runCLIWithConfigAllDrivesAllowError(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()

	return runCLICore(t, cfgPath, env, "", args...)
}

// runCLIWithConfigForDrive runs the CLI with a specific --drive flag.
func runCLIWithConfigForDrive(t *testing.T, cfgPath string, env map[string]string, driveID string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, driveID, args...)
	require.NoErrorf(t, err, "CLI command %v (drive=%s) failed\nstdout: %s\nstderr: %s",
		args, driveID, stdout, stderr)

	return stdout, stderr
}

// cleanupRemoteFolderForDrive is like cleanupRemoteFolder but for a specific drive.
func cleanupRemoteFolderForDrive(t *testing.T, driveID, folder string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, "", nil, driveID, "rm", "-r", "/"+folder)
	if err == nil || isRemoteNotFoundCleanup(stderr) {
		return
	}

	t.Errorf(
		"cleanup remote folder failed for drive=%s folder=%s\nstdout: %s\nstderr: %s\nerr: %v",
		driveID,
		folder,
		stdout,
		stderr,
		err,
	)
}

func openDriveStateDBForSyncTest(t *testing.T, env map[string]string) *sql.DB {
	t.Helper()

	dataHome := env["XDG_DATA_HOME"]
	require.NotEmpty(t, dataHome)

	sanitizedDrive := strings.ReplaceAll(drive, ":", "_")
	dbPath := filepath.Join(dataHome, "onedrive-go", "state_"+sanitizedDrive+".db")
	require.FileExists(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	return db
}

func requireBaselinePathPresent(t *testing.T, env map[string]string, relPath string) {
	t.Helper()

	db := openDriveStateDBForSyncTest(t, env)

	var count int
	err := db.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM baseline WHERE path = ?`,
		relPath,
	).Scan(&count)
	require.NoError(t, err)
	require.Equalf(t, 1, count, "expected baseline row for %s", relPath)
}
