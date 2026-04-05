//go:build e2e

package e2e

import (
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

	"github.com/tonimelisma/onedrive-go/testutil"
)

var (
	binaryPath string
	drive      string
	drive2     string // optional second drive for multi-drive E2E tests
	logDir     string
)

func TestMain(m *testing.M) {
	// Load .env and validate safety guards before anything else.
	root := findModuleRoot()
	testutil.LoadDotEnv(filepath.Join(root, ".env"))

	drive = os.Getenv("ONEDRIVE_TEST_DRIVE")
	if drive == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_TEST_DRIVE not set (check .env or env vars)")
		os.Exit(1)
	}

	testutil.ValidateAllowlist("ONEDRIVE_TEST_DRIVE")

	// Optional second drive for multi-drive E2E tests. Validated against
	// allowlist when set; multi-drive tests skip gracefully when empty.
	drive2 = os.Getenv("ONEDRIVE_TEST_DRIVE_2")
	if drive2 != "" {
		testutil.ValidateAllowlist("ONEDRIVE_TEST_DRIVE_2")
		fmt.Fprintf(os.Stderr, "E2E multi-drive: drive2=%s\n", drive2)
	}

	// Set up directory isolation (must be after drive is set, before binary build).
	cleanupIsolation := setupIsolation()

	// Build binary to temp dir.
	tmpDir, err := os.MkdirTemp("", "onedrive-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}

	binaryPath = filepath.Join(tmpDir, "onedrive-go")

	cmd := exec.Command("go", "build", "-o", binaryPath, ".")
	cmd.Dir = findModuleRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building binary: %v\n", err)
		os.Exit(1)
	}

	// Set up debug log directory for E2E visibility.
	if dir := os.Getenv("E2E_LOG_DIR"); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
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

	// Run tests, then clean up explicitly. os.Exit does not run defers,
	// so we must call cleanup before exiting to preserve rotated tokens.
	code := m.Run()
	cleanupIsolation()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}

// findModuleRoot walks up from the current dir to find go.mod.
func findModuleRoot() string {
	// Fallback to ".." — e2e/ is one level below module root.
	return testutil.FindModuleRoot("..")
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

// writeMinimalConfig writes a config file with drive but no sync_dir (uses defaults).
func writeMinimalConfig(t *testing.T) string {
	t.Helper()

	content := fmt.Sprintf("[%q]\n", drive)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	return cfgPath
}

// skipIfPersonalDrive skips the test if the primary test drive is a Personal
// OneDrive account. Some Graph API endpoints (e.g., /special/recyclebin) are
// only available on Business/SharePoint drives.
func skipIfPersonalDrive(t *testing.T) {
	t.Helper()

	if strings.HasPrefix(drive, "personal:") {
		t.Skip("skipping: test requires Business/SharePoint drive (personal drive does not support this API)")
	}
}

// --- CLI execution helpers ---

// makeCmd creates an exec.Cmd for the CLI binary with explicit environment.
// envOverrides (if non-nil) are applied on top of the current process env,
// ensuring child processes have deterministic environments regardless of
// any concurrent env mutations. When nil, the process env is inherited as-is.
func makeCmd(args []string, envOverrides map[string]string) *exec.Cmd {
	cmd := exec.Command(binaryPath, args...)
	if len(envOverrides) > 0 {
		env := os.Environ()
		for k, v := range envOverrides {
			found := false
			prefix := k + "="
			for i, e := range env {
				if strings.HasPrefix(e, prefix) {
					env[i] = prefix + v
					found = true
					break
				}
			}
			if !found {
				env = append(env, prefix+v)
			}
		}
		cmd.Env = env
	}
	return cmd
}

// runCLIWithConfigExpectError runs the CLI with a config file and expects failure.
func runCLIWithConfigExpectError(t *testing.T, cfgPath string, env map[string]string, args ...string) string {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, drive, args...)

	require.Error(t, err, "expected CLI to fail for args %v, but it succeeded\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout + stderr
}

// pollTimeout is the default timeout for polling helpers waiting on Graph API
// eventual consistency. 30 seconds covers observed propagation delays.
const pollTimeout = 30 * time.Second

// transientGraphRetryTimeout covers the rare case where a live Graph read-only
// command hits repeated 502/503/504 gateway failures before succeeding.
const transientGraphRetryTimeout = 4 * time.Minute

// pollBackoff returns exponential backoff: 500ms, 1s, 2s, 4s cap.
func pollBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond
	d := base << uint(attempt)

	cap := 4 * time.Second
	if d > cap {
		return cap
	}

	return d
}

// pollCLIWithConfigContains retries a CLI command with a config file until
// stdout contains the expected string or timeout is reached.
// env overrides (if non-nil) are applied to the child process environment.
func pollCLIWithConfigContains(
	t *testing.T, cfgPath string, env map[string]string, expected string, timeout time.Duration, args ...string,
) (string, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, args...)
		if err == nil && strings.Contains(stdout, expected) {
			return stdout, stderr
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollCLIWithConfigContains: timed out",
				"after %v waiting for %q in output of %v\nlast stdout: %s\nlast stderr: %s",
				timeout, expected, args, stdout, stderr)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// pollCLIWithConfigSuccess retries a CLI command with a config file until
// it succeeds (exit 0).
// env overrides (if non-nil) are applied to the child process environment.
func pollCLIWithConfigSuccess(t *testing.T, cfgPath string, env map[string]string, timeout time.Duration, args ...string) (string, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, args...)
		if err == nil {
			return stdout, stderr
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollCLIWithConfigSuccess: timed out",
				"after %v waiting for success of %v\nlast stdout: %s\nlast stderr: %s",
				timeout, args, stdout, stderr)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

func isRetryableGraphGatewayFailure(stderr string) bool {
	return strings.Contains(stderr, "graph: HTTP 502") ||
		strings.Contains(stderr, "graph: HTTP 503") ||
		strings.Contains(stderr, "graph: HTTP 504")
}

// pollCLIWithConfigRetryingTransientGraphFailures retries only when a live CLI
// read fails due to an upstream Graph gateway error. Real command regressions
// still fail immediately instead of being retried until timeout.
func pollCLIWithConfigRetryingTransientGraphFailures(
	t *testing.T, cfgPath string, env map[string]string, driveID string, timeout time.Duration, args ...string,
) (string, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLICore(t, cfgPath, env, driveID, args...)
		if err == nil {
			return stdout, stderr
		}

		if !isRetryableGraphGatewayFailure(stderr) {
			require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s",
				args, stdout, stderr)
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollCLIWithConfigRetryingTransientGraphFailures: timed out",
				"after %v waiting for success of %v through transient Graph failures\nlast stdout: %s\nlast stderr: %s",
				timeout, args, stdout, stderr)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// pollCLIWithConfigNotContains retries a CLI command until stdout does NOT
// contain the unexpected string (or the command errors). Used to wait for
// Graph API eventual consistency after deletions.
func pollCLIWithConfigNotContains(
	t *testing.T, cfgPath string, env map[string]string, unexpected string, timeout time.Duration, args ...string,
) (string, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, args...)
		if err == nil && !strings.Contains(stdout, unexpected) {
			return stdout, stderr
		}

		if time.Now().After(deadline) {
			require.Failf(t, "pollCLIWithConfigNotContains: timed out",
				"after %v waiting for %q to disappear from output of %v\nlast stdout: %s\nlast stderr: %s",
				timeout, unexpected, args, stdout, stderr)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

// --- File operation tests ---
//
// These top-level tests mutate the shared remote drive, so they stay
// sequential even under `go test -parallel` to keep the fast E2E suite
// deterministic.

func TestE2E_RoundTrip(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("onedrive-go-e2e-%d", time.Now().UnixNano())
	testSubfolder := testFolder + "/subfolder"
	testFile := testFolder + "/test.txt"
	testContent := []byte("Hello from onedrive-go E2E test!\n")

	// Cleanup at the end — delete the test folder.
	t.Cleanup(func() {
		fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
		cmd := makeCmd(fullArgs, nil)
		_ = cmd.Run()
	})

	t.Run("whoami", func(t *testing.T) {
		stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
			t, cfgPath, nil, drive, transientGraphRetryTimeout, "whoami", "--json",
		)

		var out map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out))
		assert.Contains(t, out, "user")
		assert.Contains(t, out, "drives")

		drives, ok := out["drives"].([]interface{})
		require.True(t, ok)
		assert.NotEmpty(t, drives)
	})

	t.Run("ls_root", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/")
		assert.Contains(t, stdout, "NAME")
	})

	t.Run("mkdir", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testSubfolder)
		assert.Contains(t, stderr, "Created")
	})

	t.Run("put", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "e2e-upload-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write(testContent)
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", tmpFile.Name(), "/"+testFile)
		assert.Contains(t, stderr, "Uploaded")
	})

	t.Run("ls_folder", func(t *testing.T) {
		// Poll for eventual consistency after put.
		stdout, _ := pollCLIWithConfigContains(t, cfgPath, nil, "test.txt", pollTimeout, "ls", "/"+testFolder)
		assert.Contains(t, stdout, "subfolder")
	})

	t.Run("stat", func(t *testing.T) {
		// Poll for eventual consistency after put.
		stdout, _ := pollCLIWithConfigContains(t, cfgPath, nil, "test.txt", pollTimeout, "stat", "/"+testFile)
		assert.Contains(t, stdout, fmt.Sprintf("%d bytes", len(testContent)))
	})

	t.Run("get", func(t *testing.T) {
		tmpDir := t.TempDir()
		localPath := filepath.Join(tmpDir, "downloaded.txt")

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "get", "/"+testFile, localPath)
		assert.Contains(t, stderr, "Downloaded")

		downloaded, err := os.ReadFile(localPath)
		require.NoError(t, err)
		assert.Equal(t, testContent, downloaded)
	})

	t.Run("rm_file", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "rm", "/"+testFile)
		assert.Contains(t, stderr, "Deleted")
	})

	t.Run("rm_subfolder", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "rm", "-r", "/"+testSubfolder)
		assert.Contains(t, stderr, "Deleted")
	})

	t.Run("rm_permanent", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "e2e-perm-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write([]byte("permanent delete test\n"))
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		permFile := testFolder + "/perm-test.txt"
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", tmpFile.Name(), "/"+permFile)
		assert.Contains(t, stderr, "Uploaded")

		// Poll until the file is visible before attempting permanent delete.
		pollCLIWithConfigContains(t, cfgPath, nil, "perm-test.txt", pollTimeout, "stat", "/"+permFile)

		_, stderr = runCLIWithConfig(t, cfgPath, nil, "rm", "--permanent", "/"+permFile)
		assert.Contains(t, stderr, "Permanently deleted")
	})

	t.Run("whoami_text", func(t *testing.T) {
		stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
			t, cfgPath, nil, drive, transientGraphRetryTimeout, "whoami",
		)

		email := strings.SplitN(drive, ":", 2)[1]
		assert.Contains(t, stdout, email, "whoami text output should contain the account email")
	})

	t.Run("status", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "status")
		assert.Contains(t, stdout, "Account:", "status should show account header")
		assert.Contains(t, stdout, "Auth:", "status should show auth state")
	})
}

// TestE2E_ErrorCases verifies that the CLI returns non-zero exit codes
// and meaningful error messages for invalid operations.
func TestE2E_ErrorCases(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("ls_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "ls", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("get_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "get", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("rm_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "rm", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("rm_folder_without_recursive", func(t *testing.T) {
		testFolder := fmt.Sprintf("onedrive-go-e2e-rmfolder-%d", time.Now().UnixNano())

		t.Cleanup(func() {
			fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
			cmd := makeCmd(fullArgs, nil)
			_ = cmd.Run()
		})

		runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "rm", "/"+testFolder)
		assert.Contains(t, output, "recursive")
	})
}

// TestE2E_JSONOutput validates that --json flags produce well-formed JSON
// with the expected schema for ls and stat commands.
func TestE2E_JSONOutput(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("ls_json", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "--json", "/")

		var items []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &items),
			"ls --json output should be valid JSON array, got: %s", stdout)

		require.NotEmpty(t, items, "expected at least one item in root listing")

		for i, item := range items {
			assert.Contains(t, item, "name", "item %d missing 'name' key", i)
			assert.Contains(t, item, "id", "item %d missing 'id' key", i)
		}
	})

	t.Run("stat_json", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "stat", "--json", "/")

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
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("put_quiet_suppresses_output", func(t *testing.T) {
		testFolder := fmt.Sprintf("onedrive-go-e2e-quiet-%d", time.Now().UnixNano())
		remotePath := "/" + testFolder + "/quiet-test.txt"

		t.Cleanup(func() {
			fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
			cmd := makeCmd(fullArgs, nil)
			_ = cmd.Run()
		})

		runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)

		tmpFile, err := os.CreateTemp("", "e2e-quiet-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write([]byte("quiet test content\n"))
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", "--quiet", tmpFile.Name(), remotePath)
		assert.Empty(t, stderr, "expected no stderr output with --quiet flag")
	})
}
