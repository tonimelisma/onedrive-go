//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// drive list E2E tests (fast — run on every CI push under the "e2e" tag)
//
// These tests exercise the `drive list` command against live OneDrive
// accounts. No sync operations, just output verification.
// ---------------------------------------------------------------------------

// Validates: R-3.3.2
// TestE2E_DriveList_HappyPath_Text verifies the basic text output of
// `drive list` when a drive is configured with a sync directory.
func TestE2E_DriveList_HappyPath_Text(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed\nstdout: %s", stdout)

	// Configured drives section must be present with drive info.
	assert.Contains(t, stdout, "Configured drives:",
		"should show configured drives header")
	assert.Contains(t, stdout, drive,
		"should show the configured drive's canonical ID")
	assert.Contains(t, stdout, "ready",
		"configured drive should show ready state")
	assert.Contains(t, stdout, syncDir,
		"configured drive should show sync directory")
}

// Validates: R-3.3.10
// TestE2E_DriveList_JSON verifies that `drive list --json` produces valid
// JSON with the expected schema.
func TestE2E_DriveList_JSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err, "drive list --json should succeed\nstdout: %s", stdout)

	var result struct {
		Configured []struct {
			CanonicalID string `json:"canonical_id"`
			State       string `json:"state"`
			Source      string `json:"source"`
			SyncDir     string `json:"sync_dir"`
		} `json:"configured"`
		Available json.RawMessage `json:"available"`
	}

	require.NoError(t, json.Unmarshal([]byte(stdout), &result),
		"drive list --json output should be valid JSON, got: %s", stdout)

	// At least one configured drive.
	require.NotEmpty(t, result.Configured,
		"configured array should have at least one entry")

	entry := result.Configured[0]
	assert.NotEmpty(t, entry.CanonicalID, "canonical_id should be set")
	assert.Equal(t, "ready", entry.State, "state should be ready")
	assert.Equal(t, "configured", entry.Source, "source should be configured")
	assert.Equal(t, syncDir, entry.SyncDir, "sync_dir should match")

	// Available array must exist (may be empty).
	assert.NotNil(t, result.Available,
		"available field should be present in JSON output")
}

// Validates: R-3.3.2
// TestE2E_DriveList_NoAccounts verifies that `drive list` shows the
// empty-state message when no accounts/tokens are present.
func TestE2E_DriveList_NoAccounts(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath, env := writeSyncConfigNoDrive(t)

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed even with no accounts\nstdout: %s", stdout)

	assert.Contains(t, stdout, "No drives configured",
		"should show the no-accounts guidance message")
	assert.Contains(t, stdout, "login",
		"should guide user to run login")
	assert.NotContains(t, stdout, "Configured drives:",
		"should NOT show configured drives header when there are none")
}

// Validates: R-3.3.2
// TestE2E_DriveList_AccountsNoDrives verifies that `drive list` shows
// available drives discovered from the network when tokens are present
// but no drive sections are configured.
func TestE2E_DriveList_AccountsNoDrives(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	// Set up per-test isolation: tokens copied but no drive section in config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("# no drives configured\n"), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed\nstdout: %s", stdout)

	// No configured drives section.
	assert.NotContains(t, stdout, "Configured drives:",
		"should not show configured drives header")

	// Available drives should be discovered from the network.
	assert.Contains(t, stdout, "Available drives (not configured):",
		"should show available drives discovered from tokens")

	// Footer with add command hint.
	assert.Contains(t, stdout, "drive add",
		"should show footer with add command hint")
}

// Validates: R-6.7.11
func TestE2E_DriveList_PersonalAccountDoesNotDuplicateCanonicalDrive(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	if !strings.HasPrefix(drive, "personal:") {
		t.Skip("skipping: test requires a Personal account")
	}

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("# no drives configured\n"), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err, "drive list --json should succeed\nstdout: %s", stdout)

	var result struct {
		Available []struct {
			CanonicalID string `json:"canonical_id"`
		} `json:"available"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))

	matchCount := 0
	for _, entry := range result.Available {
		if entry.CanonicalID == drive {
			matchCount++
		}
	}

	assert.Equal(t, 1, matchCount, "personal drive discovery should expose the canonical personal drive only once")
}

// Validates: R-3.3.2
// TestE2E_DriveList_ConfiguredNoSyncDir verifies that a configured drive
// without sync_dir still appears in the output with "(not set)".
func TestE2E_DriveList_ConfiguredNoSyncDir(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	// writeMinimalConfig creates a config with drive but no sync_dir.
	// It uses TestMain isolation (not per-test), which is fine for read-only tests.
	cfgPath := writeMinimalConfig(t)

	stdout, _, err := runCLICore(t, cfgPath, nil, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed\nstdout: %s", stdout)

	assert.Contains(t, stdout, "Configured drives:",
		"should show configured drives header")
	assert.Contains(t, stdout, drive,
		"should show the configured drive's canonical ID")
	assert.Contains(t, stdout, "ready",
		"configured drive should show ready state")
}

// Validates: R-4.8.4
// TestE2E_DriveList_ConfigTolerance verifies that `drive list` succeeds
// (exit 0) even when the config file contains unknown keys. Informational
// commands use lenient config loading that collects errors as warnings.
func TestE2E_DriveList_ConfigTolerance(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()

	// Set up per-test isolation with an unknown key in the config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("unknown_global_key = \"should warn not crash\"\n\n[%q]\nsync_dir = %q\n", drive, syncDir)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed despite unknown config key\nstdout: %s", stdout)

	assert.Contains(t, stdout, "Configured drives:",
		"should show configured drives header despite unknown config key")
}

// Validates: R-4.8.4
// TestE2E_Status_ConfigTolerance verifies that `status` succeeds (exit 0)
// even when the config file contains unknown keys. Status uses lenient
// config loading (LoadOrDefaultLenient + skipConfigAnnotation).
func TestE2E_Status_ConfigTolerance(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()

	// Per-test isolation with an unknown key in the config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("unknown_global_key = \"should warn not crash\"\n\n[%q]\nsync_dir = %q\n", drive, syncDir)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	stdout, _, err := runCLICore(t, cfgPath, env, "", "status")
	require.NoError(t, err, "status should succeed despite unknown config key\nstdout: %s", stdout)

	// Status output should contain account/auth information.
	assert.Contains(t, stdout, "Account:",
		"status should show account header despite unknown config key")
	assert.Contains(t, stdout, "Auth:",
		"status should show auth state despite unknown config key")
}

// Validates: R-4.8.4
// TestE2E_Whoami_ConfigTolerance verifies that `whoami` succeeds (exit 0)
// even when the config file contains unknown keys. Whoami uses lenient
// config loading (LoadOrDefaultLenient + skipConfigAnnotation).
func TestE2E_Whoami_ConfigTolerance(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()

	// Per-test isolation with an unknown key in the config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("unknown_global_key = \"should warn not crash\"\n\n[%q]\nsync_dir = %q\n", drive, syncDir)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", transientGraphRetryTimeout, "whoami",
	)

	// Whoami output should contain user/account information.
	assert.NotEmpty(t, stdout,
		"whoami should produce output despite unknown config key")
}

// Validates: R-6.7.11
func TestE2E_Whoami_PersonalAccountShowsSinglePersonalDrive(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	if !strings.HasPrefix(drive, "personal:") {
		t.Skip("skipping: test requires a Personal account")
	}

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", transientGraphRetryTimeout, "whoami", "--json",
	)

	var result struct {
		Drives []struct {
			DriveType string `json:"drive_type"`
		} `json:"drives"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))

	personalCount := 0
	for _, driveInfo := range result.Drives {
		if driveInfo.DriveType == "personal" {
			personalCount++
		}
	}

	assert.Equal(t, 1, personalCount, "whoami should show exactly one personal drive for Personal accounts")
}
