//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/syncverify"
	"github.com/tonimelisma/onedrive-go/testutil"
)

type statusJSON struct {
	Accounts []statusAccountJSON `json:"accounts"`
	Summary  statusSummaryJSON   `json:"summary"`
}

type statusSummaryJSON struct {
	TotalDrives int `json:"total_drives"`
}

type statusAccountJSON struct {
	Email  string            `json:"email"`
	Drives []statusDriveJSON `json:"drives"`
}

type statusDriveJSON struct {
	CanonicalID string               `json:"canonical_id"`
	SyncState   *statusSyncStateJSON `json:"sync_state,omitempty"`
}

type statusSyncStateJSON struct {
	IssueGroups          []statusIssueGroupJSON      `json:"issue_groups"`
	DeleteSafety         []statusDeleteSafetyJSON    `json:"delete_safety"`
	DeleteSafetyTotal    int                         `json:"delete_safety_total"`
	Conflicts            []statusConflictJSON        `json:"conflicts"`
	ConflictsTotal       int                         `json:"conflicts_total"`
	ConflictHistory      []statusConflictHistoryJSON `json:"conflict_history"`
	ConflictHistoryTotal int                         `json:"conflict_history_total"`
	NextActions          []string                    `json:"next_actions"`
	ExamplesLimit        int                         `json:"examples_limit"`
	Verbose              bool                        `json:"verbose"`
	StateStoreStatus     string                      `json:"state_store_status"`
}

type statusIssueGroupJSON struct {
	Title string   `json:"title"`
	Paths []string `json:"paths"`
}

type statusDeleteSafetyJSON struct {
	Path       string `json:"path"`
	State      string `json:"state"`
	ActionHint string `json:"action_hint"`
}

type statusConflictJSON struct {
	ID                  string `json:"id"`
	Path                string `json:"path"`
	ConflictType        string `json:"conflict_type"`
	State               string `json:"state"`
	RequestedResolution string `json:"requested_resolution"`
	LastRequestError    string `json:"last_request_error"`
	ActionHint          string `json:"action_hint"`
}

type statusConflictHistoryJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	Resolution   string `json:"resolution"`
	ResolvedBy   string `json:"resolved_by"`
}

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
	recordCLIQuirkEvents(t, fullArgs, stderr.String(), err)
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

func queueConflictResolution(t *testing.T, cfgPath string, env map[string]string, args ...string) string {
	t.Helper()

	resolveArgs := append([]string{"resolve"}, toResolveArgs(t, args...)...)
	_, stderr := runCLIWithConfig(t, cfgPath, env, resolveArgs...)
	assert.Contains(t, stderr, "Queued", "conflict resolution should queue durable engine-owned intent")
	return stderr
}

func queueConflictResolutionAndSync(t *testing.T, cfgPath string, env map[string]string, args ...string) {
	t.Helper()

	queueConflictResolution(t, cfgPath, env, args...)
	runCLIWithConfig(t, cfgPath, env, "sync")
}

func runStatusAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (statusJSON, string, string, error) {
	t.Helper()

	statusArgs := append([]string{"status"}, args...)
	statusArgs = append(statusArgs, "--json")
	stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, statusArgs...)
	if err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	var output statusJSON
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return statusJSON{}, stdout, stderr, fmt.Errorf("decode status json: %w", err)
	}

	return output, stdout, stderr, nil
}

func runStatusAllDrivesAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (statusJSON, string, string, error) {
	t.Helper()

	statusArgs := append([]string{"status"}, args...)
	statusArgs = append(statusArgs, "--json")
	stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, statusArgs...)
	if err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	var output statusJSON
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return statusJSON{}, stdout, stderr, fmt.Errorf("decode status json: %w", err)
	}

	return output, stdout, stderr, nil
}

func readStatus(t *testing.T, cfgPath string, env map[string]string, args ...string) statusJSON {
	t.Helper()

	output, stdout, stderr, err := runStatusAllowError(t, cfgPath, env, args...)
	require.NoErrorf(t, err, "status command failed\nstdout: %s\nstderr: %s", stdout, stderr)

	return output
}

func readStatusAllDrives(t *testing.T, cfgPath string, env map[string]string, args ...string) statusJSON {
	t.Helper()

	output, stdout, stderr, err := runStatusAllDrivesAllowError(t, cfgPath, env, args...)
	require.NoErrorf(t, err, "status command failed\nstdout: %s\nstderr: %s", stdout, stderr)

	return output
}

func requireStatusDrive(
	t *testing.T,
	status statusJSON,
	canonicalID string,
) statusDriveJSON {
	t.Helper()

	for i := range status.Accounts {
		for j := range status.Accounts[i].Drives {
			driveStatus := status.Accounts[i].Drives[j]
			if driveStatus.CanonicalID == canonicalID {
				return driveStatus
			}
		}
	}

	require.FailNowf(t, "missing status drive", "canonical_id=%s", canonicalID)
	return statusDriveJSON{}
}

func readStatusSyncState(t *testing.T, cfgPath string, env map[string]string, args ...string) statusSyncStateJSON {
	t.Helper()
	return readStatusSyncStateForDrive(t, cfgPath, env, drive, args...)
}

func readStatusSyncStateForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
	args ...string,
) statusSyncStateJSON {
	t.Helper()

	status := readStatus(t, cfgPath, env, args...)
	driveStatus := requireStatusDrive(t, status, canonicalID)
	require.NotNil(t, driveStatus.SyncState, "expected sync_state for %s", canonicalID)
	return *driveStatus.SyncState
}

func pollStatusSyncState(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	timeout time.Duration,
	ready func(statusSyncStateJSON) bool,
	args ...string,
) statusSyncStateJSON {
	return pollStatusSyncStateForDrive(t, cfgPath, env, drive, timeout, ready, args...)
}

func pollStatusSyncStateForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
	timeout time.Duration,
	ready func(statusSyncStateJSON) bool,
	args ...string,
) statusSyncStateJSON {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastStatus statusSyncStateJSON
	var lastStdout string
	var lastStderr string
	var lastErr error

	for attempt := 0; ; attempt++ {
		status, stdout, stderr, err := runStatusAllowError(t, cfgPath, env, args...)
		lastStdout = stdout
		lastStderr = stderr
		lastErr = err
		if err == nil {
			driveStatus := requireStatusDrive(t, status, canonicalID)
			if driveStatus.SyncState != nil {
				lastStatus = *driveStatus.SyncState
				if ready(lastStatus) {
					return lastStatus
				}
			}
		}

		if time.Now().After(deadline) {
			require.Failf(
				t,
				"pollStatusSyncState: timed out",
				"after %v waiting for status predicate with args %v\nlast error: %v\nlast status: %+v\nlast stdout: %s\nlast stderr: %s",
				timeout,
				args,
				lastErr,
				lastStatus,
				lastStdout,
				lastStderr,
			)
		}

		time.Sleep(pollBackoff(attempt))
	}
}

func toResolveArgs(t *testing.T, args ...string) []string {
	t.Helper()

	var (
		strategy string
		target   string
		all      bool
		dryRun   bool
	)

	for _, arg := range args {
		switch arg {
		case "local", "remote", "both":
			require.Empty(t, strategy, "only one resolve strategy is supported")
			strategy = arg
		case "--all":
			all = true
		case "--dry-run":
			dryRun = true
		default:
			require.Empty(t, target, "only one conflict target is supported")
			target = arg
		}
	}

	require.NotEmpty(t, strategy, "resolve helper requires local, remote, or both strategy")

	resolveArgs := []string{strategy}
	if dryRun {
		resolveArgs = append(resolveArgs, "--dry-run")
	}
	if all {
		resolveArgs = append(resolveArgs, "--all")
	}
	if target != "" {
		resolveArgs = append(resolveArgs, target)
	}

	return resolveArgs
}

func verifyBaselineReport(t *testing.T, cfgPath string, env map[string]string) (*syncverify.Report, error) {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	cfg, err := config.Load(cfgPath, logger)
	require.NoError(t, err)

	canonicalID, driveCfg, err := config.MatchDrive(cfg, drive, logger)
	require.NoError(t, err)

	dbPath := stateDBPathForEnv(canonicalID, env)
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	if err != nil {
		return nil, err
	}
	defer func() {
		require.NoError(t, store.Close(t.Context()))
	}()

	baseline, err := store.Load(t.Context())
	if err != nil {
		return nil, fmt.Errorf("load baseline: %w", err)
	}

	tree, err := synctree.Open(driveCfg.SyncDir)
	if err != nil {
		return nil, fmt.Errorf("open sync tree: %w", err)
	}

	report, err := syncverify.VerifyBaseline(t.Context(), baseline, tree, logger)
	if err != nil {
		return nil, fmt.Errorf("verify baseline: %w", err)
	}

	return report, nil
}

func stateDBPathForEnv(canonicalID driveid.CanonicalID, env map[string]string) string {
	if canonicalID.IsZero() {
		return ""
	}

	dataDir := env["XDG_DATA_HOME"]
	if dataDir != "" {
		dataDir = filepath.Join(dataDir, "onedrive-go")
	} else {
		home := env["HOME"]
		switch runtime.GOOS {
		case "darwin":
			dataDir = filepath.Join(home, "Library", "Application Support", "onedrive-go")
		default:
			dataDir = filepath.Join(home, ".local", "share", "onedrive-go")
		}
	}

	sanitized := strings.ReplaceAll(canonicalID.String(), ":", "_")
	return filepath.Join(dataDir, "state_"+sanitized+".db")
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

const remoteFixturePutMaxAttempts = 3

// putRemoteFile uploads string content to a remote path via a temp file.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process. The helper deliberately
// retries only the command-window fresh-parent convergence families when the
// file is only fixture setup for another test. That keeps unrelated tests from
// depending on a single CLI invocation being the one where Graph finally makes
// a freshly created parent path writable again.
func putRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath, content string) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "e2e-put-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	for attempt := 0; ; attempt++ {
		stdout, stderr, putErr := runCLIWithConfigAllowError(t, cfgPath, env, "put", tmpFile.Name(), remotePath)
		if putErr == nil {
			break
		}

		decision := classifyFixturePutCommandFailure(stderr)
		if attempt >= remoteFixturePutMaxAttempts-1 || !decision.Retry {
			require.NoErrorf(t, putErr, "CLI command %v failed\nstdout: %s\nstderr: %s",
				[]string{"put", tmpFile.Name(), remotePath}, stdout, stderr)
		}
		recordLiveProviderRecurrenceEvent(
			t,
			fmt.Sprintf("put %s", remotePath),
			decision,
			quirkOutcomeRetried,
			putErr.Error(),
		)

		time.Sleep(pollBackoff(attempt))
	}

	waitForRemoteFixtureSeedVisible(t, cfgPath, env, drive, remotePath)
}

func classifyFixturePutCommandFailure(stderr string) liveProviderRecurrenceDecision {
	switch {
	case strings.Contains(stderr, "simple-upload-create-transient-404 retry exhausted"),
		strings.Contains(stderr, "upload-session-create-transient-404 retry exhausted"):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceFreshParentChildCreateLag,
			Retry:  true,
		}
	case strings.Contains(stderr, "resolving parent") &&
		strings.Contains(stderr, "remote path not yet visible"):
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceFreshParentParentPathLag,
			Retry:  true,
		}
	default:
		return liveProviderRecurrenceDecision{
			Reason: liveProviderRecurrenceUnknown,
			Retry:  false,
		}
	}
}

func TestClassifyFixturePutCommandFailure_KnownConvergenceFamilies(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentChildCreateLag,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`Error: graph: simple-upload-create-transient-404 retry exhausted after 7 attempts: graph: HTTP 404`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentChildCreateLag,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`Error: graph: upload-session-create-transient-404 retry exhausted after 6 attempts: graph: HTTP 404`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentParentPathLag,
		Retry:  true,
	}, classifyFixturePutCommandFailure(
		`Error: resolving parent "e2e-fast-conflict": remote path not yet visible: "e2e-fast-conflict"`,
	))
}

func TestClassifyFixturePutCommandFailure_RejectsUnrelatedFailures(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`Error: graph: setting mtime after simple upload: graph: simple-upload-mtime-transient-404 retry exhausted`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`Error: graph: HTTP 403: Access denied`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`Error: resolving "/e2e-fast-conflict": resolve delete target "e2e-fast-conflict" from parent "": graph: not found`,
	))
	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixturePutCommandFailure(
		`pollRemoteEventually: timed out after 30s waiting for remote fixture seed visibility for "test.txt" via [stat /folder/test.txt]`,
	))
}

// getRemoteFile downloads a remote file and returns its content as a string.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process.
func getRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath string) string {
	t.Helper()

	waitForRemoteFixtureSeedVisible(t, cfgPath, env, drive, remotePath)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "downloaded")
	deadline := time.Now().Add(remoteWritePropagationTimeout)

	var (
		lastStdout string
		lastStderr string
		lastErr    error
	)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, "get", remotePath, localPath)
		lastStdout = stdout
		lastStderr = stderr
		lastErr = err
		if err == nil {
			data, readErr := os.ReadFile(localPath)
			require.NoError(t, readErr)
			return string(data)
		}

		if time.Now().After(deadline) {
			require.NoErrorf(
				t,
				lastErr,
				"get %q did not converge after visibility wait\nstdout: %s\nstderr: %s",
				remotePath,
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
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
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
	waitForRemoteFixtureSeedVisible(t, opsCfgPath, nil, drive, remotePath)

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
	)
	assert.Contains(t, attempt.Stderr, "Mode: download-only")
	assert.Equal(t, content, downloaded)
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

// cleanupRemoteFolderForDrive is like cleanupRemoteFolder but for a specific
// drive. Cleanup is best-effort: the suite preflight already scrubs old E2E
// artifacts, so teardown should not fail a passing test because Graph lies
// during a late cleanup read.
func cleanupRemoteFolderForDrive(t *testing.T, driveID, folder string) {
	t.Helper()

	var (
		lastStdout string
		lastStderr string
		lastErr    error
	)

	for attempt := 0; attempt < 4; attempt++ {
		stdout, stderr, err := runCLICore(t, "", nil, driveID, "rm", "-r", "/"+folder)
		if err == nil || isRemoteNotFoundCleanup(stderr) {
			return
		}

		lastStdout = stdout
		lastStderr = stderr
		lastErr = err

		time.Sleep(pollBackoff(attempt))
	}

	t.Logf(
		"warning: cleanup remote folder failed after retries for drive=%s folder=%s\nstdout: %s\nstderr: %s\nerr: %v",
		driveID,
		folder,
		lastStdout,
		lastStderr,
		lastErr,
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
