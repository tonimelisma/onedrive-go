//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// Suite state set once by TestMain and read by every test.
//
//nolint:gochecknoglobals // TestMain can hand suite setup to tests only through package variables.
var (
	binaryPath          string
	drive               string
	drive2              string // optional second drive for multi-drive E2E tests
	logDir              string
	liveConfig          testutil.LiveTestConfig
	suiteTimingRecorder *e2eTimingRecorder
	suiteQuirkRecorder  *e2eQuirkRecorder
)

func TestMain(m *testing.M) {
	// Load live-test env files and validate safety guards before anything else.
	root := findModuleRoot()
	cfg, err := testutil.LoadLiveTestConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	liveConfig = cfg
	drive = cfg.PrimaryDrive
	drive2 = cfg.SecondaryDrive

	testutil.ValidateAllowlist("ONEDRIVE_TEST_DRIVE")

	// Optional second drive for multi-drive E2E tests. Validated against
	// allowlist when set; multi-drive tests skip gracefully when empty.
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
	buildSuiteBinary(binaryPath)

	// Set up debug log directory for E2E visibility.
	logDir = resolveSuiteLogDir()

	fmt.Fprintf(os.Stderr, "E2E debug logs: %s\n", logDir)
	suiteTimingRecorder, err = newE2ETimingRecorder(logDir)
	if err != nil {
		abortSuitef(cleanupIsolation, tmpDir, "creating E2E timing recorder: %v", err)
	}
	suiteQuirkRecorder, err = newE2EQuirkRecorder(logDir)
	if err != nil {
		abortSuitef(cleanupIsolation, tmpDir, "creating E2E quirk recorder: %v", err)
	}

	suiteCfgPath := filepath.Join(tmpDir, "e2e-suite-config.toml")
	if err := writeSuiteConfig(suiteCfgPath, drive, drive2); err != nil {
		abortSuitef(cleanupIsolation, tmpDir, "writing E2E suite config: %v", err)
	}
	if os.Getenv(e2eSkipSuiteScrubEnvVar) == "1" {
		fmt.Fprintf(os.Stderr, "E2E preflight scrub skipped via %s\n", e2eSkipSuiteScrubEnvVar)
	} else {
		if err := scrubRemoteSuiteArtifacts(suiteCfgPath, drive, drive2); err != nil {
			abortSuitef(cleanupIsolation, tmpDir, "scrubbing E2E remote artifacts: %v", err)
		}
	}

	// Run tests, then clean up explicitly. os.Exit does not run defers,
	// so we must call cleanup before exiting to preserve rotated tokens.
	code := m.Run()
	cleanupIsolation()
	removeSuiteTempDir(tmpDir)
	os.Exit(code)
}

// buildSuiteBinary builds the CLI under test into path, exiting the process
// on failure.
func buildSuiteBinary(path string) {
	//nolint:gosec // a fixed `go build` of this module into the suite's own temp dir
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", path, ".")
	cmd.Dir = findModuleRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building binary: %v\n", err)
		os.Exit(1)
	}
}

// abortSuitef reports a TestMain setup failure, undoes the isolation and temp
// dir set up so far, and exits. It never returns.
func abortSuitef(cleanupIsolation func(), tmpDir, format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	cleanupIsolation()
	removeSuiteTempDir(tmpDir)
	os.Exit(1)
}

// resolveSuiteLogDir returns the E2E debug log directory: E2E_LOG_DIR when
// set, otherwise a fresh temp dir. It exits the process on failure because it
// runs from TestMain, before any test can report an error.
func resolveSuiteLogDir() string {
	if dir := os.Getenv("E2E_LOG_DIR"); dir != "" {
		//nolint:gosec // E2E_LOG_DIR is set by the CI workflow or the developer running the suite
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			fmt.Fprintf(os.Stderr, "creating E2E log dir: %v\n", mkErr)
			os.Exit(1)
		}

		return dir
	}

	dir, mkErr := os.MkdirTemp("", "onedrive-e2e-logs-*")
	if mkErr != nil {
		fmt.Fprintf(os.Stderr, "creating E2E log temp dir: %v\n", mkErr)
		os.Exit(1)
	}

	return dir
}

// removeSuiteTempDir removes the suite's build temp dir, reporting (but not
// failing on) a removal error so a leaked dir is visible in the CI log.
func removeSuiteTempDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: removing E2E temp dir %s: %v\n", dir, err)
	}
}

type remoteListJSONItem struct {
	Name string `json:"name"`
}

type syncAttemptResult struct {
	Stdout string
	Stderr string
	Err    error
}

func writeSuiteConfig(path string, drives ...string) error {
	var builder strings.Builder
	for _, driveID := range drives {
		if driveID == "" {
			continue
		}

		fmt.Fprintf(&builder, "[%q]\n", driveID)
	}

	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("writing suite config %s: %w", path, err)
	}

	return nil
}

func scrubRemoteSuiteArtifacts(cfgPath string, drives ...string) error {
	for _, driveID := range drives {
		if driveID == "" {
			continue
		}

		if err := scrubRemoteDriveArtifacts(cfgPath, driveID); err != nil {
			return err
		}
	}

	return nil
}

func scrubRemoteDriveArtifacts(cfgPath string, driveID string) error {
	_, stdout, stderr, err := execCLI(cfgPath, nil, driveID, "ls", "--json", "/")
	if err != nil {
		return fmt.Errorf("listing root for preflight scrub on %s: %w\nstdout: %s\nstderr: %s", driveID, err, stdout, stderr)
	}

	var items []remoteListJSONItem
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		return fmt.Errorf("decoding root listing for preflight scrub on %s: %w\nstdout: %s", driveID, err, stdout)
	}

	for _, item := range items {
		if !isE2EArtifactName(item.Name) {
			continue
		}

		remotePath := "/" + item.Name
		fmt.Fprintf(os.Stderr, "E2E preflight scrub: drive=%s deleting %s\n", driveID, remotePath)

		_, delStdout, delStderr, delErr := execCLI(cfgPath, nil, driveID, "rm", "-r", remotePath)
		if delErr != nil && !isRemoteNotFoundCleanup(delStderr) {
			return fmt.Errorf("deleting %s during preflight scrub on %s: %w\nstdout: %s\nstderr: %s", remotePath, driveID, delErr, delStdout, delStderr)
		}
	}

	return nil
}

func isE2EArtifactName(name string) bool {
	for _, prefix := range [...]string{"e2e-", "onedrive-go-e2e"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

func isRemoteNotFoundCleanup(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "could not be found")
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

	entry := fmt.Sprintf(
		"=== %s ===\nCMD: %s\n--- STDOUT ---\n%s\n--- STDERR ---\n%s\n\n",
		time.Now().Format(time.RFC3339),
		strings.Join(args, " "),
		stdout,
		stderr,
	)
	if err := appendDebugLogChunk(debugLogPath(t.Name()), entry); err != nil {
		t.Logf("warning: cannot write debug log: %v", err)
	}
}

// registerLogDump registers a cleanup that dumps the debug log on test failure.
func registerLogDump(t *testing.T) {
	t.Helper()

	logPath := debugLogPath(t.Name())

	t.Cleanup(func() {
		if t.Failed() {
			data, err := os.ReadFile(logPath) //nolint:gosec // the per-test debug log under the suite's own log dir
			if err != nil {
				return
			}

			t.Logf("=== DEBUG LOG DUMP (%s) ===\n%s", logPath, string(data))
		} else {
			t.Logf("debug log: %s", logPath)
		}
	})
}

func debugLogPath(testName string) string {
	return filepath.Join(logDir, sanitizeTestName(testName)+".log")
}

func appendDebugLogChunk(path string, content string) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the per-test debug log under the suite's own log dir
	if err != nil {
		return fmt.Errorf("opening debug log %s: %w", path, err)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if _, err = f.WriteString(content); err != nil {
		return fmt.Errorf("writing debug log %s: %w", path, err)
	}

	return nil
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
	//nolint:gosec // runs the suite-built CLI binary; args come from the test itself
	cmd := exec.CommandContext(context.Background(), binaryPath, args...)
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

// stopDaemon sends SIGTERM to a daemon the test started and reaps it, unless
// the test body already did. It is meant for t.Cleanup, so it logs rather
// than fails: the test's own assertions have already run.
func stopDaemon(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if cmd.Process == nil || cmd.ProcessState != nil {
		return
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Logf("signaling daemon: %v", err)
	}

	waitForDaemonExit(t, cmd)
}

// waitForDaemonExit reaps a daemon after the test has signaled it. The exit
// status is logged, not asserted: these tests verify the daemon's effects, and
// a SIGTERM'd process may legitimately report a signal exit.
func waitForDaemonExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if err := cmd.Wait(); err != nil {
		t.Logf("daemon exited with: %v", err)
	}
}

// pollTimeout is the default timeout for polling helpers waiting on Graph API
// eventual consistency. 30 seconds covers observed propagation delays.
const pollTimeout = 30 * time.Second

// shortcutFixturePropagationTimeout covers manually-created Add-to-OneDrive
// shortcut fixtures. Graph can lag longer than ordinary list/stat propagation
// when surfacing either the shared target search result or the parent root
// placeholder.
const shortcutFixturePropagationTimeout = 2 * time.Minute

// shortcutFixtureProviderOmissionProofDelay is the shorter window after which a
// decoded parent-root listing plus independently reachable shared target is
// enough evidence to classify a missing shortcut placeholder as provider
// omission instead of spending the full fixture propagation budget.
const shortcutFixtureProviderOmissionProofDelay = 30 * time.Second

// remoteWritePropagationTimeout covers slower live-account propagation after
// successful writes. GitHub-hosted CI has observed newly created folders/files
// take longer than pollTimeout to become readable via list/stat.
const remoteWritePropagationTimeout = 2 * time.Minute

// remoteDeletePropagationTimeout covers eventual disappearance after remote
// deletes. Live accounts can continue listing deleted entries briefly after
// the delete command itself has succeeded.
const remoteDeletePropagationTimeout = 2 * time.Minute

// remoteScopeTransitionTimeout covers sync-scope transitions that require a
// follow-up reconcile pass before remote reads settle on the new state.
const remoteScopeTransitionTimeout = 3 * time.Minute

// transientGraphRetryTimeout covers the rare case where a live Graph read-only
// command hits repeated 502/503/504 gateway failures before succeeding.
const transientGraphRetryTimeout = 4 * time.Minute

// pollBackoff returns exponential backoff: 500ms, 1s, 2s, 4s cap.
func pollBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond
	d := base << uint(attempt)

	maxBackoff := 4 * time.Second
	if d > maxBackoff {
		return maxBackoff
	}

	return d
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
	t *testing.T, cfgPath string, env map[string]string, driveID string, args ...string,
) (string, string) {
	t.Helper()

	timeout := transientGraphRetryTimeout
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func requireSyncEventuallyConverges(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	timeout time.Duration,
	description string,
	ready func(syncAttemptResult) bool,
	args ...string,
) syncAttemptResult {
	t.Helper()

	var last syncAttemptResult
	syncArgs := append([]string{"sync"}, args...)
	startedAt := time.Now()
	deadline := time.Now().Add(timeout)
	driveID := resolveDriveSelection(env, "")

	for attempt := 0; ; attempt++ {
		last.Stdout, last.Stderr, last.Err = runCLIWithConfigAllowError(t, cfgPath, env, syncArgs...)
		if ready(last) {
			recordTimingEvent(
				t,
				timingKindSyncConvergence,
				description,
				driveID,
				syncArgs,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return last
		}

		if time.Now().After(deadline) {
			recordTimingEvent(
				t,
				timingKindSyncConvergence,
				description,
				driveID,
				syncArgs,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeTimeout,
			)
			require.Failf(
				t,
				"requireSyncEventuallyConverges: timed out",
				"%s\ntimeout: %v\nlastErr: %v\nlastStdout: %s\nlastStderr: %s",
				description,
				timeout,
				last.Err,
				last.Stdout,
				last.Stderr,
			)
		}

		sleepForLiveTestPropagation(5 * time.Second)
	}
}
