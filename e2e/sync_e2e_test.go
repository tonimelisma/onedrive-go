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

	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/syncverify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/testutil"
)

type statIDJSON struct {
	ID string `json:"id"`
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

// writeIsolatedSharedRootSyncConfig creates a per-test sync config that targets
// a freshly created remote folder via a shared-root canonical ID. Nightly
// bidirectional sync/watch coverage uses this to keep provider-owned drive-root
// churn outside the test's authority boundary while still exercising the real
// shared-root sync path.
func writeIsolatedSharedRootSyncConfig(t *testing.T, syncDir string) (string, map[string]string) {
	t.Helper()

	return writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "")
}

func writeIsolatedSharedRootSyncConfigWithOptions(
	t *testing.T,
	syncDir string,
	extraTOML string,
) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	rootFolder := fmt.Sprintf("e2e-sync-root-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, rootFolder) })

	opsCfgPath := writeMinimalConfig(t)
	mkdirRemoteFolderForDrive(t, opsCfgPath, nil, drive, "/"+rootFolder)
	stdout, _ := runCLIWithConfig(t, opsCfgPath, nil, "stat", "--json", "/"+rootFolder)

	var stat statIDJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &stat))
	require.NotEmpty(t, stat.ID, "isolated shared-root setup should resolve the remote folder item ID")

	ownerCID, err := driveid.NewCanonicalID(drive)
	require.NoError(t, err)

	catalog, err := config.LoadCatalogForDataDir(perTestDataDir)
	require.NoError(t, err)

	ownerDrive, found := catalog.DriveByCanonicalID(ownerCID)
	require.Truef(t, found, "missing catalog drive record for %s", drive)
	require.NotEmpty(t, ownerDrive.RemoteDriveID, "catalog drive record for %s should include remote drive ID", drive)

	sharedCID, err := driveid.ConstructShared(ownerCID.Email(), ownerDrive.RemoteDriveID, stat.ID)
	require.NoError(t, err)

	require.NoError(t, config.UpdateCatalogForDataDir(perTestDataDir, func(catalog *config.Catalog) error {
		catalog.UpsertDrive(&config.CatalogDrive{
			CanonicalID:           sharedCID.String(),
			OwnerAccountCanonical: drive,
			DriveType:             driveid.DriveTypeShared,
			DisplayName:           rootFolder,
			SharedOwnerEmail:      ownerCID.Email(),
			RemoteDriveID:         ownerDrive.RemoteDriveID,
		})
		return nil
	}))

	content := fmt.Sprintf(`%s["%s"]
sync_dir = %q
`, extraTOML, sharedCID.String(), syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME":     perTestData,
		"HOME":              perTestHome,
		"ONEDRIVE_GO_DRIVE": sharedCID.String(),
	}

	waitForSharedRootListingVisible(t, cfgPath, env, sharedCID.String())

	return cfgPath, env
}

func verifyBaselineReport(t *testing.T, cfgPath string, env map[string]string) (*syncverify.Report, error) {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	cfg, err := config.Load(cfgPath, logger)
	require.NoError(t, err)

	canonicalID, driveCfg, err := config.MatchDrive(cfg, resolveDriveSelection(env, ""), logger)
	require.NoError(t, err)

	dbPath := stateDBPathForEnv(canonicalID, env)
	store, err := syncengine.NewSyncStore(t.Context(), dbPath, logger)
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

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func assertConflictCopyContains(t *testing.T, localDir string, stem string, expected string) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(localDir, stem+".conflict-*"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "conflict copy should exist for %s", stem)
	assert.Equal(t, expected, string(mustReadFile(t, matches[0])))
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

const (
	remoteFixtureMkdirMaxAttempts = 3
	remoteFixturePutMaxAttempts   = 3
)

func mkdirRemoteFolder(t *testing.T, cfgPath string, env map[string]string, remotePath string) {
	t.Helper()

	mkdirRemoteFolderForDrive(t, cfgPath, env, resolveDriveSelection(env, ""), remotePath)
}

func mkdirRemoteFolderForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) {
	t.Helper()

	for attempt := 0; ; attempt++ {
		stdout, stderr, mkdirErr := runCLIWithConfigAllowErrorForDrive(
			t,
			cfgPath,
			env,
			driveID,
			"mkdir",
			remotePath,
		)
		if mkdirErr == nil {
			break
		}

		decision := classifyFixtureMkdirCommandFailure(stderr)
		if attempt >= remoteFixtureMkdirMaxAttempts-1 || !decision.Retry {
			require.NoErrorf(t, mkdirErr, "CLI command %v failed\nstdout: %s\nstderr: %s",
				[]string{"mkdir", remotePath}, stdout, stderr)
		}
		recordLiveProviderRecurrenceEvent(
			t,
			fmt.Sprintf("mkdir %s", remotePath),
			decision,
			quirkOutcomeRetried,
			mkdirErr.Error(),
		)

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}

	waitForRemoteExactStatVisible(t, cfgPath, env, driveID, remotePath)
}

// putRemoteFile uploads string content to a remote path via a temp file.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process. The helper deliberately
// retries only the command-window fresh-parent convergence families when the
// file is only fixture setup for another test. That keeps unrelated tests from
// depending on a single CLI invocation being the one where Graph finally makes
// a freshly created parent path writable again.
func putRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath, content string) {
	t.Helper()

	putRemoteFileForDrive(t, cfgPath, env, resolveDriveSelection(env, ""), remotePath, content)
}

func putRemoteFileForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
	content string,
) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "e2e-put-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	for attempt := 0; ; attempt++ {
		stdout, stderr, putErr := runCLIWithConfigAllowErrorForDrive(
			t,
			cfgPath,
			env,
			driveID,
			"put",
			tmpFile.Name(),
			remotePath,
		)
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}

	waitForRemoteFixtureSeedVisible(t, cfgPath, env, driveID, remotePath)
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

func classifyFixtureMkdirCommandFailure(stderr string) liveProviderRecurrenceDecision {
	switch {
	case strings.Contains(stderr, "remote path not yet visible"):
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

func TestClassifyFixtureMkdirCommandFailure_KnownConvergenceFamilies(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceFreshParentParentPathLag,
		Retry:  true,
	}, classifyFixtureMkdirCommandFailure(
		`Error: confirming folder "e2e-sync-bidi-123/data" visibility: remote path not yet visible: "e2e-sync-bidi-123/data"`,
	))
}

func TestClassifyFixtureMkdirCommandFailure_RejectsUnrelatedFailures(t *testing.T) {
	t.Parallel()

	assert.Equal(t, liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, classifyFixtureMkdirCommandFailure(
		`Error: creating folder "data": graph: HTTP 403: Access denied`,
	))
}

// getRemoteFile downloads a remote file and returns its content as a string.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process.
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
	mkdirRemoteFolderForDrive(t, opsCfgPath, nil, drive, "/"+testFolder)

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

	// Copy the managed inventory catalog.
	testutil.CopyCatalogFile(srcDir, dstDir)
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

	// Copy the managed inventory catalog.
	testutil.CopyCatalogFile(srcDir, dstDir)
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

		sleepForLiveTestPropagation(pollBackoff(attempt))
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

	sanitizedDrive := strings.ReplaceAll(resolveDriveSelection(env, ""), ":", "_")
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
