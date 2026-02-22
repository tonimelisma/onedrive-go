//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// deltaSleepDuration is the pause after remote modifications to allow
// the Graph API delta endpoint to reflect changes. 2 seconds is conservative.
const deltaSleepDuration = 2 * time.Second

// syncEnvOpts configures optional filter and safety settings for a sync test environment.
type syncEnvOpts struct {
	skipDotfiles        bool
	skipFiles           []string
	skipDirs            []string
	maxFileSize         string
	bigDeleteThreshold  int
	bigDeletePercentage int
	bigDeleteMinItems   int
}

// syncReport mirrors the JSON output schema from `onedrive-go sync --json`.
type syncReport struct {
	Mode           string            `json:"mode"`
	DryRun         bool              `json:"dry_run"`
	DurationMs     int64             `json:"duration_ms"`
	FoldersCreated int               `json:"folders_created"`
	Downloaded     int               `json:"downloaded"`
	BytesDown      int64             `json:"bytes_downloaded"`
	Uploaded       int               `json:"uploaded"`
	BytesUp        int64             `json:"bytes_uploaded"`
	LocalDeleted   int               `json:"local_deleted"`
	RemoteDeleted  int               `json:"remote_deleted"`
	Moved          int               `json:"moved"`
	Conflicts      int               `json:"conflicts"`
	Errors         []syncReportError `json:"errors"`
}

// syncReportError represents a single action error in the JSON report.
type syncReportError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// syncEnv provides an isolated test environment for sync E2E tests.
// Each instance has its own local sync directory, remote test folder,
// state database (isolated via HOME override), and TOML config file.
type syncEnv struct {
	t          *testing.T
	syncDir    string // local sync directory (temp)
	remoteDir  string // remote test folder path (e.g., "/sync-e2e-1234567890")
	configPath string // TOML config file path
	homeDir    string // fake HOME for state DB isolation
}

// newSyncEnv creates a fully isolated sync test environment.
// It copies the real OAuth token to a temp HOME directory so the sync engine
// gets its own state database while reusing the same auth token.
func newSyncEnv(t *testing.T, opts syncEnvOpts) *syncEnv {
	t.Helper()

	remoteDir := fmt.Sprintf("/sync-e2e-%d", time.Now().UnixNano())

	tmpRoot := t.TempDir()
	syncDir := filepath.Join(tmpRoot, "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	// Fake HOME for state DB isolation.
	homeDir := filepath.Join(tmpRoot, "home")
	fakeDataDir := platformDataDir(homeDir)
	require.NoError(t, os.MkdirAll(fakeDataDir, 0o700))

	// Copy the real token file into the fake HOME's data directory.
	realDataDir := realPlatformDataDir()
	require.NotEmpty(t, realDataDir, "cannot determine real data directory")

	tokenFile := tokenFileForDrive(drive)
	require.NotEmpty(t, tokenFile, "cannot derive token filename for drive %q", drive)

	srcToken := filepath.Join(realDataDir, tokenFile)
	dstToken := filepath.Join(fakeDataDir, tokenFile)

	tokenData, err := os.ReadFile(srcToken)
	require.NoError(t, err, "cannot read token file %s (is the drive logged in?)", srcToken)
	require.NoError(t, os.WriteFile(dstToken, tokenData, 0o600))

	// Write TOML config.
	configPath := filepath.Join(tmpRoot, "config.toml")
	writeTestConfig(t, configPath, drive, syncDir, opts)

	// Create remote test folder.
	runCLI(t, "mkdir", remoteDir)

	env := &syncEnv{
		t:          t,
		syncDir:    syncDir,
		remoteDir:  remoteDir,
		configPath: configPath,
		homeDir:    homeDir,
	}

	// Best-effort cleanup of remote folder.
	t.Cleanup(func() {
		cmd := exec.Command(binaryPath, "--drive", drive, "rm", remoteDir)
		_ = cmd.Run()
	})

	return env
}

// --- CLI runners ---

// runSyncRaw runs the sync command and returns stdout, stderr, and error.
// Does not fail on non-zero exit codes.
func (env *syncEnv) runSyncRaw(args ...string) (string, string, error) {
	env.t.Helper()

	fullArgs := []string{"--config", env.configPath, "--drive", drive, "--verbose", "sync"}
	fullArgs = append(fullArgs, args...)

	cmd := exec.Command(binaryPath, fullArgs...)
	cmd.Env = env.buildEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Always log stderr so debug output appears in go test -v, even for passing tests.
	if stderrStr := stderr.String(); stderrStr != "" {
		env.t.Logf("sync stderr:\n%s", stderrStr)
	}

	return stdout.String(), stderr.String(), err
}

// runSync runs sync expecting success and returns stdout, stderr.
func (env *syncEnv) runSync(args ...string) (string, string) {
	env.t.Helper()

	stdout, stderr, err := env.runSyncRaw(args...)
	if err != nil {
		env.t.Fatalf("sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	return stdout, stderr
}

// runSyncJSON runs sync with --json and parses the JSON report.
// Handles both success and error exit codes since JSON is written before
// the error return when the engine completes.
func (env *syncEnv) runSyncJSON(args ...string) *syncReport {
	env.t.Helper()

	jsonArgs := append([]string{"--json"}, args...)
	stdout, _, _ := env.runSyncRaw(jsonArgs...)

	var report syncReport
	require.NoError(env.t, json.Unmarshal([]byte(stdout), &report),
		"failed to parse sync JSON output: %s", stdout)

	return &report
}

// runSyncExpectError runs sync expecting a non-zero exit code.
// Returns combined stdout+stderr for assertion.
func (env *syncEnv) runSyncExpectError(args ...string) string {
	env.t.Helper()

	stdout, stderr, err := env.runSyncRaw(args...)
	require.Error(env.t, err, "expected sync to fail but it succeeded\nstdout: %s\nstderr: %s",
		stdout, stderr)

	return stdout + stderr
}

// --- Remote operations (use real HOME, no isolation needed) ---

// putRemote uploads content to a remote path under the test's remote folder.
func (env *syncEnv) putRemote(remotePath, content string) {
	env.t.Helper()

	tmpFile, err := os.CreateTemp("", "sync-e2e-put-*")
	require.NoError(env.t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	require.NoError(env.t, err)
	require.NoError(env.t, tmpFile.Close())

	runCLI(env.t, "put", tmpFile.Name(), env.remoteDir+"/"+remotePath)
}

// rmRemote deletes a remote path under the test's remote folder.
func (env *syncEnv) rmRemote(remotePath string) {
	env.t.Helper()
	runCLI(env.t, "rm", env.remoteDir+"/"+remotePath)
}

// mkdirRemote creates a remote folder under the test's remote folder.
func (env *syncEnv) mkdirRemote(remotePath string) {
	env.t.Helper()
	runCLI(env.t, "mkdir", env.remoteDir+"/"+remotePath)
}

// lsRemote lists a remote folder and returns stdout.
func (env *syncEnv) lsRemote(remotePath string) string {
	env.t.Helper()

	path := env.remoteDir
	if remotePath != "" {
		path = env.remoteDir + "/" + remotePath
	}

	stdout, _ := runCLI(env.t, "ls", path)

	return stdout
}

// --- Local operations ---

// writeLocal writes content to a file relative to the sync directory.
// Creates parent directories as needed.
func (env *syncEnv) writeLocal(relPath, content string) {
	env.t.Helper()

	fullPath := filepath.Join(env.syncDir, relPath)
	require.NoError(env.t, os.MkdirAll(filepath.Dir(fullPath), 0o700))
	require.NoError(env.t, os.WriteFile(fullPath, []byte(content), 0o644))
}

// writeLocalBytes writes binary content to a file relative to the sync directory.
func (env *syncEnv) writeLocalBytes(relPath string, data []byte) {
	env.t.Helper()

	fullPath := filepath.Join(env.syncDir, relPath)
	require.NoError(env.t, os.MkdirAll(filepath.Dir(fullPath), 0o700))
	require.NoError(env.t, os.WriteFile(fullPath, data, 0o644))
}

// readLocal reads a file relative to the sync directory.
func (env *syncEnv) readLocal(relPath string) []byte {
	env.t.Helper()

	data, err := os.ReadFile(filepath.Join(env.syncDir, relPath))
	require.NoError(env.t, err, "failed to read local file %s", relPath)

	return data
}

// localExists checks whether a file or directory exists relative to the sync directory.
func (env *syncEnv) localExists(relPath string) bool {
	env.t.Helper()

	_, err := os.Stat(filepath.Join(env.syncDir, relPath))

	return err == nil
}

// removeLocal deletes a file or directory relative to the sync directory.
func (env *syncEnv) removeLocal(relPath string) {
	env.t.Helper()
	require.NoError(env.t, os.RemoveAll(filepath.Join(env.syncDir, relPath)))
}

// findConflictFile searches for a conflict copy matching the given file stem.
// Returns the conflict file's relative path, or empty string if not found.
func (env *syncEnv) findConflictFile(relPath string) string {
	env.t.Helper()

	dir := filepath.Dir(relPath)
	ext := filepath.Ext(relPath)
	stem := strings.TrimSuffix(filepath.Base(relPath), ext)

	searchDir := filepath.Join(env.syncDir, dir)

	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		name := e.Name()
		// Conflict files look like: stem.conflict-YYYYMMDD-HHMMSS.ext
		if strings.HasPrefix(name, stem+".conflict-") && strings.HasSuffix(name, ext) {
			return filepath.Join(dir, name)
		}
	}

	return ""
}

// testPath returns a path relative to the sync root that maps to the test's
// remote folder. The sync engine maps the entire drive root to syncDir, so
// remote path /sync-e2e-NNN/foo.txt appears locally at syncDir/sync-e2e-NNN/foo.txt.
// Use this for all local operations on test files to get correct path mapping.
func (env *syncEnv) testPath(relPath string) string {
	return filepath.Join(strings.TrimPrefix(env.remoteDir, "/"), relPath)
}

// sleep pauses briefly to allow remote changes to propagate through the delta API.
func (env *syncEnv) sleep() {
	time.Sleep(deltaSleepDuration)
}

// --- Helpers ---

// buildEnv returns a copy of the current environment with HOME overridden
// to the test's fake home directory for state DB isolation.
func (env *syncEnv) buildEnv() []string {
	result := make([]string, 0, len(os.Environ()))

	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HOME=") {
			result = append(result, e)
		}
	}

	result = append(result, "HOME="+env.homeDir)

	return result
}

// platformDataDir returns the platform-specific application data directory
// for a given home directory. Mirrors config.DefaultDataDir logic.
func platformDataDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "onedrive-go")
	case "linux":
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "onedrive-go")
		}

		return filepath.Join(home, ".local", "share", "onedrive-go")
	default:
		return filepath.Join(home, ".local", "share", "onedrive-go")
	}
}

// realPlatformDataDir returns the real data directory using the actual HOME.
func realPlatformDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return platformDataDir(home)
}

// tokenFileForDrive derives the token filename from a canonical drive ID.
// Mirrors config.DriveTokenPath logic without importing the config package.
func tokenFileForDrive(canonicalID string) string {
	parts := strings.SplitN(canonicalID, ":", 3)
	if len(parts) < 2 {
		return ""
	}

	driveType := parts[0]
	email := parts[1]

	// SharePoint drives share the business token.
	if driveType == "sharepoint" {
		driveType = "business"
	}

	return "token_" + driveType + "_" + email + ".json"
}

// writeTestConfig writes a TOML config file for the sync test environment.
func writeTestConfig(t *testing.T, path, driveID, syncDir string, opts syncEnvOpts) {
	t.Helper()

	var buf bytes.Buffer

	// Global settings (before any table headers).
	if opts.skipDotfiles {
		buf.WriteString("skip_dotfiles = true\n")
	}

	if len(opts.skipFiles) > 0 {
		fmt.Fprintf(&buf, "skip_files = [%s]\n", quotedSlice(opts.skipFiles))
	}

	if len(opts.skipDirs) > 0 {
		fmt.Fprintf(&buf, "skip_dirs = [%s]\n", quotedSlice(opts.skipDirs))
	}

	if opts.maxFileSize != "" {
		fmt.Fprintf(&buf, "max_file_size = %q\n", opts.maxFileSize)
	}

	if opts.bigDeleteThreshold > 0 {
		fmt.Fprintf(&buf, "big_delete_threshold = %d\n", opts.bigDeleteThreshold)
	}

	if opts.bigDeletePercentage > 0 {
		fmt.Fprintf(&buf, "big_delete_percentage = %d\n", opts.bigDeletePercentage)
	}

	if opts.bigDeleteMinItems > 0 {
		fmt.Fprintf(&buf, "big_delete_min_items = %d\n", opts.bigDeleteMinItems)
	}

	// Drive section.
	fmt.Fprintf(&buf, "\n[%q]\n", driveID)
	fmt.Fprintf(&buf, "sync_dir = %q\n", syncDir)

	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

// quotedSlice formats a string slice as a TOML inline array: "a", "b", "c".
func quotedSlice(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}

	return strings.Join(quoted, ", ")
}
