//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

var (
	binaryPath          string
	drive               string
	drive2              string // optional second drive for multi-drive E2E tests
	logDir              string
	liveConfig          testutil.LiveTestConfig
	suiteTimingRecorder *e2eTimingRecorder
	suiteQuirkRecorder  *e2eQuirkRecorder
)

var e2eArtifactPrefixes = []string{
	"e2e-",
	"onedrive-go-e2e",
}

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
	suiteTimingRecorder, err = newE2ETimingRecorder(logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating E2E timing recorder: %v\n", err)
		cleanupIsolation()
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}
	suiteQuirkRecorder, err = newE2EQuirkRecorder(logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating E2E quirk recorder: %v\n", err)
		cleanupIsolation()
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	suiteCfgPath := filepath.Join(tmpDir, "e2e-suite-config.toml")
	if err := writeSuiteConfig(suiteCfgPath, drive, drive2); err != nil {
		fmt.Fprintf(os.Stderr, "writing E2E suite config: %v\n", err)
		cleanupIsolation()
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}
	if os.Getenv(e2eSkipSuiteScrubEnvVar) == "1" {
		fmt.Fprintf(os.Stderr, "E2E preflight scrub skipped via %s\n", e2eSkipSuiteScrubEnvVar)
	} else {
		if err := scrubRemoteSuiteArtifacts(suiteCfgPath, drive, drive2); err != nil {
			fmt.Fprintf(os.Stderr, "scrubbing E2E remote artifacts: %v\n", err)
			cleanupIsolation()
			os.RemoveAll(tmpDir)
			os.Exit(1)
		}
	}

	// Run tests, then clean up explicitly. os.Exit does not run defers,
	// so we must call cleanup before exiting to preserve rotated tokens.
	code := m.Run()
	cleanupIsolation()
	os.RemoveAll(tmpDir)
	os.Exit(code)
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

		builder.WriteString(fmt.Sprintf("[%q]\n", driveID))
	}

	return os.WriteFile(path, []byte(builder.String()), 0o600)
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
	stdout, stderr, err := runCLIProcess(cfgPath, nil, driveID, "ls", "--json", "/")
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

		delStdout, delStderr, delErr := runCLIProcess(cfgPath, nil, driveID, "rm", "-r", remotePath)
		if delErr != nil && !isRemoteNotFoundCleanup(delStderr) {
			return fmt.Errorf("deleting %s during preflight scrub on %s: %w\nstdout: %s\nstderr: %s", remotePath, driveID, delErr, delStdout, delStderr)
		}
	}

	return nil
}

func isE2EArtifactName(name string) bool {
	for _, prefix := range e2eArtifactPrefixes {
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

func debugLogPath(testName string) string {
	return filepath.Join(logDir, sanitizeTestName(testName)+".log")
}

func appendDebugLogChunk(path string, content string) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = f.WriteString(content)
	return err
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

func runCLIProcess(cfgPath string, env map[string]string, driveID string, args ...string) (string, string, error) {
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
	return stdout.String(), stderr.String(), err
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func pollRemoteEventually(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	timeout time.Duration,
	eventKind string,
	description string,
	ready func(stdout, stderr string, err error) bool,
	args ...string,
) (string, string) {
	t.Helper()

	startedAt := time.Now()
	deadline := time.Now().Add(timeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLICore(t, cfgPath, env, driveID, args...)
		if ready(stdout, stderr, err) {
			recordTimingEvent(
				t,
				eventKind,
				description,
				driveID,
				args,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return stdout, stderr
		}

		if time.Now().After(deadline) {
			recordTimingEvent(
				t,
				eventKind,
				description,
				driveID,
				args,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeTimeout,
			)
			require.Failf(t, "pollRemoteEventually: timed out",
				"after %v waiting for %s via %v\nlast stdout: %s\nlast stderr: %s",
				timeout, description, args, stdout, stderr)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

// waitForRemoteExactStatVisible is for tests whose contract is the exact path
// route itself. It should not be used for generic fixture seeding.
func waitForRemoteExactStatVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote exact-path visibility for %q", remotePath),
		func(_ string, _ string, err error) bool {
			return err == nil
		},
		"stat", remotePath,
	)
}

// waitForRemoteParentListingContains proves list-visible availability under a
// parent path without asserting exact-path convergence.
func waitForRemoteParentListingContains(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	parentPath string,
	expected string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote parent listing contains %q under %q", expected, parentPath),
		func(stdout, _ string, err error) bool {
			return err == nil && strings.Contains(stdout, expected)
		},
		"ls", parentPath,
	)
}

type fixtureSeedVisibilityOutcome uint8

const (
	fixtureSeedVisibilityKeepPolling fixtureSeedVisibilityOutcome = iota
	fixtureSeedVisibilityExactSuccess
	fixtureSeedVisibilitySoftenedByParentListing
)

// classifyFixtureSeedVisibilityAttempt keeps the fixture-readiness contract
// pure: exact stat success wins immediately, otherwise a visible parent listing
// softens the lag into the documented post-mutation destination recurrence.
func classifyFixtureSeedVisibilityAttempt(
	targetBase string,
	statErr error,
	parentListStdout string,
	parentListErr error,
) fixtureSeedVisibilityOutcome {
	switch {
	case statErr == nil:
		return fixtureSeedVisibilityExactSuccess
	case parentListErr == nil && strings.Contains(parentListStdout, targetBase):
		return fixtureSeedVisibilitySoftenedByParentListing
	default:
		return fixtureSeedVisibilityKeepPolling
	}
}

// waitForRemoteFixtureSeedVisible is the shared fixture-readiness contract for
// remote writes that are only setup for later assertions. It accepts either
// exact stat success or parent-list visibility so unrelated tests do not depend
// on one stricter read path winning a provider convergence race.
func waitForRemoteFixtureSeedVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) {
	t.Helper()

	cleanPath := path.Clean(remotePath)
	if cleanPath == "." || cleanPath == "/" || cleanPath == "" {
		return
	}

	base := path.Base(cleanPath)
	if base == "." || base == "/" || base == "" {
		return
	}

	parentPath := path.Dir(cleanPath)
	if parentPath == "." || parentPath == "" {
		parentPath = "/"
	}

	deadline := time.Now().Add(remoteWritePropagationTimeout)
	startedAt := time.Now()

	var lastStdout string
	var lastStderr string

	for attempt := 0; ; attempt++ {
		statStdout, statStderr, statErr := runCLIWithConfigAllowError(t, cfgPath, env, "stat", cleanPath)
		switch classifyFixtureSeedVisibilityAttempt(base, statErr, "", nil) {
		case fixtureSeedVisibilityExactSuccess:
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote fixture seed visibility for %q", cleanPath),
				driveID,
				[]string{"stat", cleanPath},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return
		}

		lastStdout = statStdout
		lastStderr = statStderr

		listStdout, listStderr, listErr := runCLIWithConfigAllowError(t, cfgPath, env, "ls", parentPath)
		switch classifyFixtureSeedVisibilityAttempt(base, statErr, listStdout, listErr) {
		case fixtureSeedVisibilitySoftenedByParentListing:
			recordLiveProviderRecurrenceEvent(
				t,
				fmt.Sprintf("fixture visibility %s", cleanPath),
				liveProviderRecurrenceDecision{
					Reason: liveProviderRecurrencePostMutationDestinationPathLag,
					Retry:  false,
				},
				quirkOutcomeSoftened,
				lastStderr,
			)
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote fixture seed visibility for %q", cleanPath),
				driveID,
				[]string{"ls", parentPath},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return
		}

		lastStdout = listStdout
		lastStderr = listStderr

		if time.Now().After(deadline) {
			recordTimingEvent(
				t,
				timingKindRemoteWriteVisibility,
				fmt.Sprintf("remote fixture seed visibility for %q", cleanPath),
				driveID,
				[]string{"stat", cleanPath, "ls", parentPath},
				remoteWritePropagationTimeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeTimeout,
			)
			require.Failf(
				t,
				"waitForRemoteFixtureSeedVisible: timed out",
				"after %v waiting for fixture visibility of %q via stat %q or ls %q\nlast stdout: %s\nlast stderr: %s",
				remoteWritePropagationTimeout,
				cleanPath,
				cleanPath,
				parentPath,
				lastStdout,
				lastStderr,
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func TestClassifyFixtureSeedVisibilityAttempt(t *testing.T) {
	t.Parallel()

	assert.Equal(t, fixtureSeedVisibilityExactSuccess, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		nil,
		"",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilitySoftenedByParentListing, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"test.txt\nother.txt\n",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilityKeepPolling, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"other.txt\n",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilityKeepPolling, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"",
		assert.AnError,
	))
}

func waitForRemoteDeleteDisappearance(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	unexpected string,
	args ...string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteDeletePropagationTimeout,
		timingKindRemoteDeleteDisappearance,
		fmt.Sprintf("remote delete disappearance for %q", unexpected),
		func(stdout, stderr string, err error) bool {
			return deleteDisappearanceReady(stdout, stderr, err, unexpected)
		},
		args...,
	)
}

func deleteDisappearanceReady(stdout string, stderr string, err error, unexpected string) bool {
	switch {
	case err == nil:
		return !strings.Contains(stdout, unexpected)
	case isRemoteNotFoundCleanup(stderr):
		return true
	default:
		return false
	}
}

func waitForRemoteScopeTransition(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	expected string,
	args ...string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteScopeTransitionTimeout,
		timingKindRemoteScopeTransition,
		fmt.Sprintf("remote scope transition for %q", expected),
		func(stdout, _ string, err error) bool {
			return err == nil && strings.Contains(stdout, expected)
		},
		args...,
	)
}

func TestDeleteDisappearanceReady(t *testing.T) {
	t.Parallel()

	assert.True(t, deleteDisappearanceReady("other.txt\n", "", nil, "target.txt"))
	assert.False(t, deleteDisappearanceReady("target.txt\n", "", nil, "target.txt"))
	assert.True(t, deleteDisappearanceReady("", "The resource could not be found.", assert.AnError, "target.txt"))
	assert.False(t, deleteDisappearanceReady("", "transport timeout", assert.AnError, "target.txt"))
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

	for attempt := 0; ; attempt++ {
		last.Stdout, last.Stderr, last.Err = runCLIWithConfigAllowError(t, cfgPath, env, syncArgs...)
		if ready(last) {
			recordTimingEvent(
				t,
				timingKindSyncConvergence,
				description,
				"",
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
				"",
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

	return last
}
