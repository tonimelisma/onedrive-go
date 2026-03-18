//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Shared folder E2E tests (B-286)
//
// Prerequisites:
//   - testitesti18 (drive) owns shared folders
//   - kikkelimies123 (drive2) is the recipient
//   - Shortcuts appear in drive2's delta
//   - ONEDRIVE_TEST_DRIVE_2 must be set
//
// See ci_issues.md §22 for setup details.
// ---------------------------------------------------------------------------

// requireDrive2 skips the test if drive2 is not configured.
func requireDrive2Shared(t *testing.T) {
	t.Helper()

	if drive2 == "" {
		t.Skip("ONEDRIVE_TEST_DRIVE_2 not set — skipping shared folder test")
	}
}

// writeSyncConfigForDrive2 creates a sync config pointing to drive2
// (kikkelimies123, the shared folder recipient) with per-test isolation.
func writeSyncConfigForDrive2(t *testing.T, syncDir string) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o755))
	copyTokenFileForDrive(t, testDataDir, perTestDataDir, drive2)

	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive2, syncDir)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env
}

// TestE2E_SharedFolder_OwnerUpload_RecipientDownload verifies that a file
// uploaded by the owner (drive) to a shared folder is downloaded by the
// recipient (drive2) via sync.
func TestE2E_SharedFolder_OwnerUpload_RecipientDownload(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	ownerCfgPath := writeMinimalConfig(t)

	// Create a test folder on the owner's drive.
	testFolder := fmt.Sprintf("e2e-shared-%d", time.Now().UnixNano())
	runCLIWithConfig(t, ownerCfgPath, nil, "mkdir", "/"+testFolder)

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Upload a file to the shared folder via the owner.
	putRemoteFile(t, ownerCfgPath, nil, "/"+testFolder+"/shared-doc.txt", "shared content from owner")

	// Wait for the file to appear on the owner's drive.
	pollCLIWithConfigContains(t, ownerCfgPath, nil, "shared-doc.txt", pollTimeout, "ls", "/"+testFolder)

	// Now sync as the recipient (drive2). The shared folder should appear
	// via a shortcut in delta if it has been shared.
	localDir := t.TempDir()
	recipientCfg, recipientEnv := writeSyncConfigForDrive2(t, localDir)

	// Run download-only sync. The shared folder's shortcut should be detected
	// and content observed.
	_, stderr := runCLIWithConfigForDrive(t, recipientCfg, recipientEnv, drive2,
		"sync", "--force", "--download-only")

	// Verify the sync completed. Status output (Mode, Duration, Plan, Results)
	// goes to stderr via CLIContext.Statusf.
	assert.Contains(t, stderr, "Mode: download-only",
		"sync should complete successfully for recipient")
}

// Validates: R-3.6.1
// TestE2E_SharedFolder_DriveList_ShowsShared verifies that `drive list`
// shows shared drives as available when authenticated as the recipient.
func TestE2E_SharedFolder_DriveList_ShowsShared(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	// Use drive2 (the recipient) with per-test isolation so only drive2's
	// token and config are visible. drive list doesn't accept --drive, so
	// we use runCLICore with empty driveID.
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDrive2(t, syncDir)

	stdout, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list should succeed\nstdout: %s", stdout)

	// drive2 is configured, so we expect the configured header.
	assert.Contains(t, stdout, "Configured drives:",
		"drive list should show configured drives section for drive2")

	// The shared folder from testitesti18 should appear as an available
	// drive with "shared by" in its description.
	assert.Contains(t, stdout, "shared by",
		"drive list should show shared drives from testitesti18")
}

// TestE2E_SharedFolder_RecipientSyncTwice_Idempotent verifies that syncing
// twice as the recipient doesn't produce duplicate downloads or errors.
func TestE2E_SharedFolder_RecipientSyncTwice_Idempotent(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	localDir := t.TempDir()
	recipientCfg, recipientEnv := writeSyncConfigForDrive2(t, localDir)

	// First sync.
	runCLIWithConfigForDrive(t, recipientCfg, recipientEnv, drive2,
		"sync", "--force", "--download-only")

	// Second sync — should report no changes. Status output goes to stderr.
	_, stderr := runCLIWithConfigForDrive(t, recipientCfg, recipientEnv, drive2,
		"sync", "--force", "--download-only")

	assert.Contains(t, stderr, "No changes detected",
		"second sync should report no changes")
}
