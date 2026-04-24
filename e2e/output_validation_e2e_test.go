//go:build e2e && e2e_full

package e2e

import (
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

// TestE2E_Status_JSONShape validates that status --json emits one top-level
// summary plus nested per-mount sync-state sections.
func TestE2E_Status_JSONShape(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-out-statusjson-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a file and sync so the per-drive status read model has baseline data.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "good.txt"), []byte("good"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	status := readStatus(t, cfgPath, env)
	assert.Equal(t, 1, status.Summary.TotalMounts)

	mountStatus := requireStatusMount(t, status, drive)
	require.NotNil(t, mountStatus.SyncState)
	assert.Equal(t, 5, mountStatus.SyncState.ExamplesLimit)
	assert.False(t, mountStatus.SyncState.Verbose)
}

func TestE2E_Status_FilteredDriveIsSubsetOfAllDrives(t *testing.T) {
	t.Parallel()
	requireDrive2(t)
	registerLogDump(t)

	syncDir1 := t.TempDir()
	syncDir2 := t.TempDir()
	cfgPath, env := writeMultiDriveConfig(t, syncDir1, syncDir2)

	statusAll := readStatusAllDrives(t, cfgPath, env)
	assert.Equal(t, 2, statusAll.Summary.TotalMounts)
	requireStatusMount(t, statusAll, drive)
	requireStatusMount(t, statusAll, drive2)

	statusFiltered := readStatus(t, cfgPath, env)
	assert.Equal(t, 1, statusFiltered.Summary.TotalMounts)
	requireStatusMount(t, statusFiltered, drive)

	for i := range statusFiltered.Accounts {
		for j := range statusFiltered.Accounts[i].Mounts {
			assert.NotEqual(t, drive2, statusFiltered.Accounts[i].Mounts[j].CanonicalID)
		}
	}
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
