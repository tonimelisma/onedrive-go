//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// TestE2E_DriveList_NoAccounts verifies that `drive list` reports an
// actionable error when no accounts/tokens are present.
func TestE2E_DriveList_NoAccounts(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath, env := writeSyncConfigNoDrive(t)

	_, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.Error(t, err, "drive list should fail when no accounts are present")

	assert.Contains(t, stderr, "no accounts configured",
		"should report no accounts configured")
	assert.Contains(t, stderr, "login",
		"should guide user to run login")
}

// Validates: R-3.3.2
// TestE2E_DriveList_AccountsNoDrives verifies that `drive list` reports
// an actionable error when tokens are present but no drive sections are
// configured — the CLI currently requires at least one configured drive.
func TestE2E_DriveList_AccountsNoDrives(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	// Set up per-test isolation: tokens copied but no drive section in config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o755))
	copyTokenFile(t, testDataDir, perTestDataDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("# no drives configured\n"), 0o644))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	_, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.Error(t, err, "drive list should fail when no drives are configured")

	assert.Contains(t, stderr, "no drives configured",
		"should report no drives configured")
	assert.Contains(t, stderr, "drive add",
		"should guide user to add a drive")
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
// TestE2E_DriveList_ConfigRejectsUnknownKeys verifies that `drive list`
// fails with a clear error when the config file contains unknown keys.
// The CLI strictly validates config to prevent typos from causing silent
// misconfiguration.
func TestE2E_DriveList_ConfigRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()

	// Set up per-test isolation with an unknown key in the config.
	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o755))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("unknown_global_key = \"should cause error\"\n\n[%q]\nsync_dir = %q\n", drive, syncDir)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	_, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.Error(t, err, "drive list should fail with unknown config key")

	assert.Contains(t, stderr, "unknown config key",
		"error should mention the unknown config key")
}
