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

// --- e2eMode abstraction for dual-mode testing ---

// e2eMode wraps CLI execution for a specific config mode (no config vs with config).
type e2eMode struct {
	name        string
	run         func(t *testing.T, args ...string) (string, string)
	expectError func(t *testing.T, args ...string) string
}

// fileOpModes returns both no-config and with-config modes for parametrized tests.
func fileOpModes(t *testing.T) []e2eMode {
	t.Helper()

	// with-config mode: minimal config with drive section pointing to a temp state dir.
	cfgPath := writeMinimalConfig(t)

	return []e2eMode{
		{
			name:        "no_config",
			run:         runCLI,
			expectError: runCLIExpectError,
		},
		{
			name: "with_config",
			run: func(t *testing.T, args ...string) (string, string) {
				t.Helper()
				return runCLIWithConfig(t, cfgPath, args...)
			},
			expectError: func(t *testing.T, args ...string) string {
				t.Helper()
				return runCLIWithConfigExpectError(t, cfgPath, args...)
			},
		},
	}
}

// writeMinimalConfig writes a config file with drive but no sync_dir (uses defaults).
func writeMinimalConfig(t *testing.T) string {
	t.Helper()

	content := fmt.Sprintf("[%q]\n", drive)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	return cfgPath
}

// --- CLI execution helpers ---

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

// runCLIWithConfigExpectError runs the CLI with a config file and expects failure.
func runCLIWithConfigExpectError(t *testing.T, cfgPath string, args ...string) string {
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

	require.Error(t, err, "expected CLI to fail for args %v, but it succeeded\nstdout: %s\nstderr: %s",
		args, stdout.String(), stderr.String())

	return stdout.String() + stderr.String()
}

// pollCLIContains retries a CLI command until stdout contains the expected
// string or timeout is reached. Handles Graph API eventual consistency.
func pollCLIContains(
	t *testing.T, expected string, timeout time.Duration, args ...string,
) (string, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
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

		if err == nil && strings.Contains(stdout.String(), expected) {
			return stdout.String(), stderr.String()
		}

		if time.Now().After(deadline) {
			t.Fatalf("pollCLIContains: timed out after %v waiting for %q in output of %v\nlast stdout: %s\nlast stderr: %s",
				timeout, expected, args, stdout.String(), stderr.String())
		}

		time.Sleep(pollBackoff(attempt))
	}
}

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

// --- Parametrized tests ---

func TestE2E_RoundTrip(t *testing.T) {
	modes := fileOpModes(t)

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			testFolder := fmt.Sprintf("onedrive-go-e2e-%s-%d", mode.name, time.Now().UnixNano())
			testSubfolder := testFolder + "/subfolder"
			testFile := testFolder + "/test.txt"
			testContent := []byte("Hello from onedrive-go E2E test!\n")

			// Cleanup at the end — delete the test folder.
			t.Cleanup(func() {
				fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
				cmd := exec.Command(binaryPath, fullArgs...)
				_ = cmd.Run()
			})

			t.Run("whoami", func(t *testing.T) {
				stdout, _ := mode.run(t, "whoami", "--json")

				var out map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(stdout), &out))
				assert.Contains(t, out, "user")
				assert.Contains(t, out, "drives")

				drives, ok := out["drives"].([]interface{})
				require.True(t, ok)
				assert.NotEmpty(t, drives)
			})

			t.Run("ls_root", func(t *testing.T) {
				stdout, _ := mode.run(t, "ls", "/")
				assert.Contains(t, stdout, "NAME")
			})

			t.Run("mkdir", func(t *testing.T) {
				_, stderr := mode.run(t, "mkdir", "/"+testSubfolder)
				assert.Contains(t, stderr, "Created")
			})

			t.Run("put", func(t *testing.T) {
				tmpFile, err := os.CreateTemp("", "e2e-upload-*")
				require.NoError(t, err)
				defer os.Remove(tmpFile.Name())

				_, err = tmpFile.Write(testContent)
				require.NoError(t, err)
				require.NoError(t, tmpFile.Close())

				_, stderr := mode.run(t, "put", tmpFile.Name(), "/"+testFile)
				assert.Contains(t, stderr, "Uploaded")
			})

			t.Run("ls_folder", func(t *testing.T) {
				stdout, _ := mode.run(t, "ls", "/"+testFolder)
				assert.Contains(t, stdout, "test.txt")
				assert.Contains(t, stdout, "subfolder")
			})

			t.Run("stat", func(t *testing.T) {
				stdout, _ := mode.run(t, "stat", "/"+testFile)
				assert.Contains(t, stdout, "test.txt")
				assert.Contains(t, stdout, fmt.Sprintf("%d bytes", len(testContent)))
			})

			t.Run("get", func(t *testing.T) {
				tmpDir := t.TempDir()
				localPath := filepath.Join(tmpDir, "downloaded.txt")

				_, stderr := mode.run(t, "get", "/"+testFile, localPath)
				assert.Contains(t, stderr, "Downloaded")

				downloaded, err := os.ReadFile(localPath)
				require.NoError(t, err)
				assert.Equal(t, testContent, downloaded)
			})

			t.Run("rm_file", func(t *testing.T) {
				_, stderr := mode.run(t, "rm", "/"+testFile)
				assert.Contains(t, stderr, "Deleted")
			})

			t.Run("rm_subfolder", func(t *testing.T) {
				_, stderr := mode.run(t, "rm", "-r", "/"+testSubfolder)
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
				_, stderr := mode.run(t, "put", tmpFile.Name(), "/"+permFile)
				assert.Contains(t, stderr, "Uploaded")

				_, stderr = mode.run(t, "rm", "--permanent", "/"+permFile)
				assert.Contains(t, stderr, "Permanently deleted")
			})

			t.Run("whoami_text", func(t *testing.T) {
				stdout, _ := mode.run(t, "whoami")

				email := strings.SplitN(drive, ":", 2)[1]
				assert.Contains(t, stdout, email, "whoami text output should contain the account email")
			})

			t.Run("status", func(t *testing.T) {
				stdout, _ := mode.run(t, "status")
				assert.Contains(t, stdout, "Account:", "status should show account header")
				assert.Contains(t, stdout, "Token:", "status should show token state")
			})
		})
	}
}

// TestE2E_ErrorCases verifies that the CLI returns non-zero exit codes
// and meaningful error messages for invalid operations.
func TestE2E_ErrorCases(t *testing.T) {
	modes := fileOpModes(t)

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("ls_not_found", func(t *testing.T) {
				output := mode.expectError(t, "ls", "/nonexistent-uuid-path-12345")
				assert.Contains(t, output, "nonexistent-uuid-path-12345")
			})

			t.Run("get_root_is_folder", func(t *testing.T) {
				output := mode.expectError(t, "get", "/")
				assert.Contains(t, output, "folder")
			})

			t.Run("rm_not_found", func(t *testing.T) {
				output := mode.expectError(t, "rm", "/nonexistent-uuid-path-12345")
				assert.Contains(t, output, "nonexistent-uuid-path-12345")
			})

			t.Run("rm_folder_without_recursive", func(t *testing.T) {
				testFolder := fmt.Sprintf("onedrive-go-e2e-rmfolder-%s-%d", mode.name, time.Now().UnixNano())

				t.Cleanup(func() {
					fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
					cmd := exec.Command(binaryPath, fullArgs...)
					_ = cmd.Run()
				})

				mode.run(t, "mkdir", "/"+testFolder)
				output := mode.expectError(t, "rm", "/"+testFolder)
				assert.Contains(t, output, "recursive")
			})
		})
	}
}

// TestE2E_JSONOutput validates that --json flags produce well-formed JSON
// with the expected schema for ls and stat commands.
func TestE2E_JSONOutput(t *testing.T) {
	modes := fileOpModes(t)

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("ls_json", func(t *testing.T) {
				stdout, _ := mode.run(t, "ls", "--json", "/")

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
				stdout, _ := mode.run(t, "stat", "--json", "/")

				var obj map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(stdout), &obj),
					"stat --json output should be valid JSON object, got: %s", stdout)

				assert.Contains(t, obj, "name", "stat JSON missing 'name' key")
				assert.Contains(t, obj, "id", "stat JSON missing 'id' key")
			})
		})
	}
}

// TestE2E_QuietFlag verifies that --quiet suppresses informational output
// on stderr during file operations.
func TestE2E_QuietFlag(t *testing.T) {
	modes := fileOpModes(t)

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("put_quiet_suppresses_output", func(t *testing.T) {
				testFolder := fmt.Sprintf("onedrive-go-e2e-quiet-%s-%d", mode.name, time.Now().UnixNano())
				remotePath := "/" + testFolder + "/quiet-test.txt"

				t.Cleanup(func() {
					fullArgs := []string{"--drive", drive, "rm", "-r", "/" + testFolder}
					cmd := exec.Command(binaryPath, fullArgs...)
					_ = cmd.Run()
				})

				mode.run(t, "mkdir", "/"+testFolder)

				tmpFile, err := os.CreateTemp("", "e2e-quiet-*")
				require.NoError(t, err)
				defer os.Remove(tmpFile.Name())

				_, err = tmpFile.Write([]byte("quiet test content\n"))
				require.NoError(t, err)
				require.NoError(t, tmpFile.Close())

				_, stderr := mode.run(t, "put", "--quiet", tmpFile.Name(), remotePath)
				assert.Empty(t, stderr, "expected no stderr output with --quiet flag")
			})
		})
	}
}
