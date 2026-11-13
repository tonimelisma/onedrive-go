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
	"regexp"
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
	suiteDriveRoles     testutil.DriveRoles
	suiteTimingRecorder *e2eTimingRecorder
	suiteQuirkRecorder  *e2eQuirkRecorder

	// suiteStandingConditions is what a sync of the suite's drives leaves
	// unsynced before any test has created anything.
	suiteStandingConditions testutil.StandingSyncConditions
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
	suiteDriveRoles = testutil.NewDriveRoles(drive, drive2)

	testutil.ValidateAllowlist("ONEDRIVE_TEST_DRIVE")

	// Optional second drive for multi-drive E2E tests. Validated against
	// allowlist when set; multi-drive tests skip gracefully when empty. The
	// suite's output names the variable, never the drive: CI logs are public.
	if drive2 != "" {
		testutil.ValidateAllowlist("ONEDRIVE_TEST_DRIVE_2")
		fmt.Fprintln(os.Stderr, "E2E multi-drive: ONEDRIVE_TEST_DRIVE_2 is set")
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
	recordStandingSyncConditions(cleanupIsolation, tmpDir, drive, drive2)

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

// scrubRemoteDriveArtifacts deletes what earlier runs left in a drive's
// root. Its output names the drive by role: it runs from TestMain, whose
// output reaches the public CI log.
func scrubRemoteDriveArtifacts(cfgPath string, driveID string) error {
	role := suiteDriveRoles.Role(driveID)
	_, stdout, stderr, err := execCLI(cfgPath, nil, driveID, "ls", "--json", "/")
	if err != nil {
		return fmt.Errorf("listing root for preflight scrub on %s: %w\nstdout: %s\nstderr: %s",
			role, err, suiteDriveRoles.Redact(stdout), suiteDriveRoles.Redact(stderr))
	}

	var items []remoteListJSONItem
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		return fmt.Errorf("decoding root listing for preflight scrub on %s: %w\nstdout: %s", role, err, suiteDriveRoles.Redact(stdout))
	}

	for _, item := range items {
		if !isE2EArtifactName(item.Name) {
			continue
		}

		remotePath := "/" + item.Name
		fmt.Fprintf(os.Stderr, "E2E preflight scrub: drive=%s deleting %s\n", role, remotePath)

		_, delStdout, delStderr, delErr := execCLI(cfgPath, nil, driveID, "rm", "-r", remotePath)
		if delErr != nil && !isRemoteNotFoundCleanup(delStderr) {
			return fmt.Errorf("deleting %s during preflight scrub on %s: %w\nstdout: %s\nstderr: %s",
				remotePath, role, delErr, suiteDriveRoles.Redact(delStdout), suiteDriveRoles.Redact(delStderr))
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

	if err := appendDebugLogChunk(debugLogPath(t.Name()), cliExecutionLogEntry(args, stdout, stderr)); err != nil {
		t.Logf("warning: cannot write debug log: %v", err)
	}
}

// cliExecutionLogEntry is the debug-log record of one CLI invocation.
func cliExecutionLogEntry(args []string, stdout, stderr string) string {
	return fmt.Sprintf(
		"=== %s ===\nCMD: %s\n--- STDOUT ---\n%s\n--- STDERR ---\n%s\n\n",
		time.Now().Format(time.RFC3339),
		strings.Join(args, " "),
		stdout,
		stderr,
	)
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
	return filepath.Join(logDir, debugLogFileName(testName))
}

// debugLogFileName is a test's debug log file name: the test's name, any
// configured drive in it replaced by its role first, because the file is a CI
// artifact of a public repository and its name goes with it.
func debugLogFileName(testName string) string {
	return sanitizeTestName(suiteDriveRoles.Redact(testName)) + ".log"
}

// appendDebugLogChunk appends content to a per-test debug log, with every
// configured drive named by role: the logs are uploaded as CI artifacts of a
// public repository, where log masking does not reach.
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

	if _, err = f.WriteString(suiteDriveRoles.Redact(content)); err != nil {
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

// daemonPollTimeout is the default timeout for polling in daemon tests.
// Daemon operations may take longer than one-shot sync due to poll intervals.
const daemonPollTimeout = 3 * time.Minute

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

// syncConvergePollInterval is the pause between two passes of
// requireSyncEventuallyConverges: long enough for delta to move on.
const syncConvergePollInterval = 5 * time.Second

// syncRunner is the drive selection the converge helpers run their one-shot
// sync passes against: the suite's default drive, one named drive, or every
// configured drive at once. It hands back a pass's exit error instead of
// failing the test, so each helper decides what a failed pass means.
//
// A one-shot sync in a live test either goes through one of the helpers built
// on a runner, or stays a single call with a "Single pass by design:" comment
// saying what that one pass proves (an "Uploaded ..." or "Up to date" line, a
// conflict, a dry run, an idempotence check). A converge loop would hide a
// regression where a pass that should finish in one go needs two, so a site
// whose proof is the pass itself never moves into one.
type syncRunner struct {
	cfgPath string
	env     map[string]string
	// driveID is the resolved --drive value; empty runs every configured
	// drive (no --drive flag).
	driveID string
}

// syncRunnerForDrive runs against one drive; an empty driveID is the suite's
// default drive, as in runCLIWithConfig.
func syncRunnerForDrive(cfgPath string, env map[string]string, driveID string) syncRunner {
	return syncRunner{cfgPath: cfgPath, env: env, driveID: resolveDriveSelection(env, driveID)}
}

// syncRunnerAllDrives runs every configured drive in one pass, as
// runCLIWithConfigAllDrives does.
func syncRunnerAllDrives(cfgPath string, env map[string]string) syncRunner {
	return syncRunner{cfgPath: cfgPath, env: env}
}

func (r syncRunner) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	return runCLICore(t, r.cfgPath, r.env, r.driveID, args...)
}

// requireSyncEventuallyConverges runs `sync args...` on the default drive
// until ready holds for a pass, for state that reaches the client only
// eventually (a remote change not yet in delta). A pass's non-zero exit is
// handed to ready, which decides.
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

	return requireSyncEventuallyConvergesWith(t, syncRunnerForDrive(cfgPath, env, ""), timeout, description, ready, args...)
}

// requireSyncEventuallyConvergesForDrive is requireSyncEventuallyConverges
// against one named drive.
func requireSyncEventuallyConvergesForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	timeout time.Duration,
	description string,
	ready func(syncAttemptResult) bool,
	args ...string,
) syncAttemptResult {
	t.Helper()

	return requireSyncEventuallyConvergesWith(t, syncRunnerForDrive(cfgPath, env, driveID), timeout, description, ready, args...)
}

// requireSyncEventuallyConvergesAllDrives is requireSyncEventuallyConverges
// with every configured drive in each pass.
func requireSyncEventuallyConvergesAllDrives(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	timeout time.Duration,
	description string,
	ready func(syncAttemptResult) bool,
	args ...string,
) syncAttemptResult {
	t.Helper()

	return requireSyncEventuallyConvergesWith(t, syncRunnerAllDrives(cfgPath, env), timeout, description, ready, args...)
}

func requireSyncEventuallyConvergesWith(
	t *testing.T,
	runner syncRunner,
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
		last.Stdout, last.Stderr, last.Err = runner.run(t, syncArgs...)
		requireLeftWorkExitsNonZero(t, last)
		if ready(last) {
			recordTimingEvent(t, timingKindSyncConvergence, description, runner.driveID, syncArgs,
				timeout, time.Since(startedAt), attempt+1, timingOutcomeSuccess)
			return last
		}

		if time.Now().After(deadline) {
			recordTimingEvent(t, timingKindSyncConvergence, description, runner.driveID, syncArgs,
				timeout, time.Since(startedAt), attempt+1, timingOutcomeTimeout)
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

		sleepForLiveTestPropagation(syncConvergePollInterval)
	}
}

// syncLeftWorkPattern matches the one-shot summary of a drive whose run left
// work: an action failed, work was left for the next sync, or the store
// still holds a condition that keeps a file unsynced. Such a run exits 1
// (R-6.6.28).
var syncLeftWorkPattern = regexp.MustCompile(`\d+ failed|will be retried on the next sync|still not synced`)

// syncLeftWorkErrorPattern matches the command error of a one-shot sync that
// left work, for one selected drive or for several.
var syncLeftWorkErrorPattern = regexp.MustCompile(`left \d+ files? unsynced|drives left files unsynced`)

// syncPassFinishedWork reports whether a pass exited zero and left no work.
// A requireSyncEventuallyConverges predicate that waits for remote state and
// then reads what the same pass uploaded checks it, so the loop also runs
// through a transient failure on another action.
func syncPassFinishedWork(result syncAttemptResult) bool {
	return result.Err == nil && !syncLeftWorkPattern.MatchString(result.Stdout)
}

// requireLeftWorkExitsNonZero checks one half of the one-shot contract on
// every pass: a summary that says the run left work comes with a non-zero
// exit (R-6.6.28). The other half, that a non-zero exit comes with such a
// summary, holds only where no drive error is expected; the helpers that
// need a pass to succeed check it themselves.
func requireLeftWorkExitsNonZero(t *testing.T, result syncAttemptResult) {
	t.Helper()

	if syncLeftWorkPattern.MatchString(result.Stdout) {
		require.Errorf(t, result.Err, "a one-shot sync whose summary says it left work must exit non-zero\nstdout: %s\nstderr: %s",
			result.Stdout, result.Stderr)
	}
}

// syncRetryableWorkPattern matches the part of a one-shot summary a later
// pass can finish without the user: failed actions and work left for the
// next sync. A condition the store holds ("still not synced") is not in it.
var syncRetryableWorkPattern = regexp.MustCompile(`\d+ failed|will be retried on the next sync`)

// runSyncUntilOnlyConditionsRemain runs a one-shot sync on the default drive
// for a test that made a condition the scan records on purpose (a name or a
// path OneDrive refuses). It runs again while a pass left work a later pass
// can finish (a transient failure), and returns the first pass whose only
// left work is what the store still holds. That pass exits 1: a file stays
// unsynced (R-6.6.28). It returns that pass's summary (stdout) and debug log
// (stderr).
func runSyncUntilOnlyConditionsRemain(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	runner := syncRunnerForDrive(cfgPath, env, "")
	syncArgs := append([]string{"sync"}, args...)
	deadline := time.Now().Add(transientGraphRetryTimeout)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runner.run(t, syncArgs...)
		if !syncRetryableWorkPattern.MatchString(stdout) {
			requireSyncLeftWork(t, stdout, stderr, err)
			return stdout, stderr
		}
		requireLeftWorkExitsNonZero(t, syncAttemptResult{Stdout: stdout, Stderr: stderr, Err: err})

		if time.Now().After(deadline) {
			require.Failf(t, "runSyncUntilOnlyConditionsRemain: timed out",
				"%v still left retryable work after %v and %d passes\nlast stdout: %s\nlast stderr: %s",
				syncArgs, transientGraphRetryTimeout, attempt+1, stdout, stderr)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

// requireSyncExitMatchesSummary checks the one-shot contract on one pass: it
// exits non-zero exactly when its summary says it left work (R-6.6.28). A
// non-zero exit without that summary is a drive that could not sync, which
// fails the test. It reports whether the pass left work.
func requireSyncExitMatchesSummary(
	t *testing.T,
	runner syncRunner,
	syncArgs []string,
	stdout, stderr string,
	err error,
) bool {
	t.Helper()

	// --quiet drops the summary; the command error still says it.
	leftWork := syncLeftWorkPattern.MatchString(stdout) || syncLeftWorkErrorPattern.MatchString(stderr)
	require.Equalf(t, leftWork, err != nil,
		"CLI command %s (drive=%s): a one-shot sync exits non-zero exactly when its summary says it left work\n"+
			"err: %v\nstdout: %s\nstderr: %s",
		suiteDriveRoles.Redact(fmt.Sprint(syncArgs)), suiteDriveRoles.Role(runner.driveID), err, stdout, stderr)

	return leftWork
}

// runSyncPass runs one one-shot sync pass on the default drive for a test
// whose proof is that pass itself (a line it prints, a tree it leaves alone,
// a session it resumes). The pass may leave work a transient failure causes:
// it then exits 1 and says so, and the test's own assertion on the pass
// decides. A pass that exits non-zero without saying it left work is a drive
// that could not sync and fails the test. It returns the pass's summary
// (stdout) and debug log (stderr).
func runSyncPass(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	return runSyncPassWith(t, syncRunnerForDrive(cfgPath, env, ""), args...)
}

// runSyncPassForDrive is runSyncPass against one named drive.
func runSyncPassForDrive(t *testing.T, cfgPath string, env map[string]string, driveID string, args ...string) (string, string) {
	t.Helper()

	return runSyncPassWith(t, syncRunnerForDrive(cfgPath, env, driveID), args...)
}

// runSyncPassAllDrives is runSyncPass with every configured drive.
func runSyncPassAllDrives(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	return runSyncPassWith(t, syncRunnerAllDrives(cfgPath, env), args...)
}

func runSyncPassWith(t *testing.T, runner syncRunner, args ...string) (string, string) {
	t.Helper()

	syncArgs := append([]string{"sync"}, args...)
	stdout, stderr, err := runner.run(t, syncArgs...)
	requireSyncExitMatchesSummary(t, runner, syncArgs, stdout, stderr, err)

	return stdout, stderr
}

// syncExitLeftWork is the exit status of a one-shot sync that finished but
// left work (R-6.6.28, R-6.6.31).
const syncExitLeftWork = 1

// requireSyncLeftWork checks that a one-shot sync pass finished but left
// work: it exits 1, its summary says what was left, and its error says the
// run left files unsynced.
func requireSyncLeftWork(t *testing.T, stdout, stderr string, err error) {
	t.Helper()

	var exitErr *exec.ExitError
	require.ErrorAsf(t, err, &exitErr, "a sync that left work must exit non-zero\nstdout: %s\nstderr: %s", stdout, stderr)
	require.Equalf(t, syncExitLeftWork, exitErr.ExitCode(), "stderr: %s", stderr)
	require.Regexp(t, syncLeftWorkPattern, stdout)
	require.Contains(t, stderr, "unsynced", "the command error says the run left files unsynced")
}

// runSyncThroughTransientFailures runs a one-shot sync on the default drive
// that must finish its work, and runs it again while a pass left work (a
// dropped connection or a 5xx on one transfer). One live sync is not a proof
// that every action ran: a test that asserts the remote result of a single
// pass fails on the network's bad minute rather than on the client. Every
// pass must say it left work exactly when it exits non-zero (R-6.6.28): an
// exit without that summary is a drive that could not sync and fails the
// test at once. Work that does not clear (a condition the test made on
// purpose) fails the test at the deadline; a test that expects one runs its
// sync with runCLIWithConfigAllowError and asserts the exit instead. It
// returns the last pass's summary (stdout) and the debug log (stderr) of
// every pass, in order. It does nothing for delta lag: a remote change not
// yet in delta needs requireSyncEventuallyConverges.
func runSyncThroughTransientFailures(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string) {
	t.Helper()

	return runSyncThroughTransientFailuresWith(t, syncRunnerForDrive(cfgPath, env, ""), args...)
}

// runSyncThroughTransientFailuresForDrive is runSyncThroughTransientFailures
// against one named drive.
func runSyncThroughTransientFailuresForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	args ...string,
) (string, string) {
	t.Helper()

	return runSyncThroughTransientFailuresWith(t, syncRunnerForDrive(cfgPath, env, driveID), args...)
}

// runSyncThroughTransientFailuresAllDrives is runSyncThroughTransientFailures
// with every configured drive in each pass; one drive's left work repeats
// the pass for all of them.
func runSyncThroughTransientFailuresAllDrives(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string) {
	t.Helper()

	return runSyncThroughTransientFailuresWith(t, syncRunnerAllDrives(cfgPath, env), args...)
}

func runSyncThroughTransientFailuresWith(t *testing.T, runner syncRunner, args ...string) (string, string) {
	t.Helper()

	var stderrAll strings.Builder
	syncArgs := append([]string{"sync"}, args...)
	startedAt := time.Now()
	deadline := startedAt.Add(transientGraphRetryTimeout)
	description := "sync completes its work through transient failures"

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runner.run(t, syncArgs...)
		leftWork := requireSyncExitMatchesSummary(t, runner, syncArgs, stdout, stderr, err)
		stderrAll.WriteString(stderr)

		if !leftWork {
			if attempt > 0 {
				recordTimingEvent(t, timingKindSyncConvergence, description, runner.driveID, syncArgs,
					transientGraphRetryTimeout, time.Since(startedAt), attempt+1, timingOutcomeSuccess)
			}
			return stdout, stderrAll.String()
		}

		if time.Now().After(deadline) {
			recordTimingEvent(t, timingKindSyncConvergence, description, runner.driveID, syncArgs,
				transientGraphRetryTimeout, time.Since(startedAt), attempt+1, timingOutcomeTimeout)
			require.Failf(t, "runSyncThroughTransientFailures: timed out",
				"%v still left work after %v and %d passes\nlast stdout: %s\nlast stderr: %s",
				syncArgs, transientGraphRetryTimeout, attempt+1, stdout, stderr)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}
