//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Output validation E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests validate output formats: JSON schema, quiet mode suppression,
// multi-drive report structure, and status edge cases.
// ---------------------------------------------------------------------------

// TestE2E_Status_DetailedJSON validates that single-drive status --json emits
// the detailed read model arrays used for issues, delete safety, and conflicts.
func TestE2E_Status_DetailedJSON(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-out-statusjson-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync so the detailed status read model has baseline data.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "good.txt"), []byte("good"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status", "--json")

	var output map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &output),
		"status --json should produce valid JSON, got: %s", stdout)

	assert.Contains(t, output, "issue_groups")
	assert.Contains(t, output, "delete_safety")
	assert.Contains(t, output, "conflicts")
	assert.Contains(t, output, "state_store_status")
	assert.Equal(t, "healthy", output["state_store_status"])
}

// TestE2E_Status_NoDrives validates that status with no configured drives
// shows guidance about adding drives.
func TestE2E_Status_NoDrives(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	cfgPath, env := writeSyncConfigNoDrive(t)

	// Run status without --drive (all-drives mode).
	stdout, _, err := runCLICore(t, cfgPath, env, "", "status")

	// May succeed or fail depending on implementation — either way,
	// the output should guide the user to add a drive.
	combined := stdout
	if err != nil {
		// Status may fail when no drives are configured.
		combined = stdout
	}

	assert.Contains(t, combined, "login",
		"status with no drives should suggest 'login' or 'drive add'")
}

// TestE2E_Sync_QuietMode validates that --quiet suppresses informational
// output during sync operations.
func TestE2E_Sync_QuietMode(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-out-quiet-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "quiet.txt"), []byte("quiet test"), 0o600))

	// Sync with --quiet.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--quiet", "--upload-only")

	// Quiet mode should suppress DEBUG and INFO level log lines.
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		assert.NotContains(t, line, "level=DEBUG",
			"quiet mode should not emit DEBUG lines: %s", line)
		assert.NotContains(t, line, "level=INFO",
			"quiet mode should not emit INFO lines: %s", line)
	}
}

// TestE2E_Sync_MultiDriveReport validates that syncing multiple drives
// produces per-drive report headers with separator lines. Requires drive2.
func TestE2E_Sync_MultiDriveReport(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	testFolder1 := fmt.Sprintf("e2e-out-multi1-%d", time.Now().UnixNano())
	testFolder2 := fmt.Sprintf("e2e-out-multi2-%d", time.Now().UnixNano())

	// Create files in both sync dirs.
	localDir1 := filepath.Join(syncDir1, testFolder1)
	require.NoError(t, os.MkdirAll(localDir1, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir1, "multi1.txt"), []byte("drive 1 content"), 0o644))

	localDir2 := filepath.Join(syncDir2, testFolder2)
	require.NoError(t, os.MkdirAll(localDir2, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir2, "multi2.txt"), []byte("drive 2 content"), 0o644))

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder1)
		cleanupRemoteFolderForDrive(t, drive2, testFolder2)
	})

	// Sync all drives.
	_, stderr := runCLIWithConfigAllDrives(t, cfgPath, env, "sync", "--upload-only")

	// Multi-drive output should have per-drive headers with "---" separators.
	assert.Contains(t, stderr, "---", "multi-drive report should have separator headers")
}
