//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// drive list E2E tests (slow — run nightly under "e2e && e2e_full" tags)
//
// These tests require sync operations, drive remove, or heavy API calls
// that make them too slow for every-push CI.
// ---------------------------------------------------------------------------

// Validates: R-3.3.4
// TestE2E_DriveList_AllFlag verifies that `drive list --all` discovers at
// least as many available drives as the default (capped) listing.
func TestE2E_DriveList_AllFlag(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Text mode: both default and --all should succeed and show configured drives.
	defaultListOutput, _, err := runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err, "drive list (text) should succeed\nstdout: %s", printable(defaultListOutput))
	assertOutputContains(t, defaultListOutput, "Configured drives:",
		"text default should show configured drives header")

	allDrivesOutput, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--all")
	require.NoError(t, err, "drive list --all (text) should succeed\nstdout: %s", printable(allDrivesOutput))
	assertOutputContains(t, allDrivesOutput, "Configured drives:",
		"text --all should show configured drives header")

	// Default listing (capped SharePoint discovery).
	defaultOut, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err, "drive list --json should succeed\nstdout: %s", printable(defaultOut))

	var defaultResult driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(defaultOut), &defaultResult),
		"default JSON should parse, got: %s", printable(defaultOut))

	expectedAvailable := make([]string, 0, len(defaultResult.Available))
	for i := range defaultResult.Available {
		expectedAvailable = append(expectedAvailable, defaultResult.Available[i].CanonicalID)
	}

	if len(expectedAvailable) == 0 {
		return
	}

	deadline := time.Now().Add(pollTimeout)
	var lastOut string
	var lastStderr string

	for attempt := 0; ; attempt++ {
		allOut, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, "drive", "list", "--json", "--all")
		lastOut = allOut
		lastStderr = stderr
		if err != nil {
			if !isRetryableGraphGatewayFailure(stderr) {
				require.NoError(t, err, "drive list --json --all should succeed\nstdout: %s\nstderr: %s", printable(allOut), printable(stderr))
			}
		} else {
			var allResult driveListE2EOutput
			require.NoError(t, json.Unmarshal([]byte(allOut), &allResult),
				"--all JSON should parse, got: %s", printable(allOut))

			containsAll := true
			for _, canonicalID := range expectedAvailable {
				if !driveListAvailableContainsCanonicalID(allResult, canonicalID) {
					containsAll = false
					break
				}
			}
			if containsAll {
				return
			}
		}

		if time.Now().After(deadline) {
			t.Skipf(
				"live drive list --json --all did not expose the default-visible available drives within %v; Graph search omitted them on this run\nexpected canonical ids: %v\nlast stdout: %s\nlast stderr: %s",
				pollTimeout,
				suiteDriveRoles.Redact(fmt.Sprint(expectedAvailable)),
				printable(lastOut),
				printable(lastStderr),
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

// Validates: R-3.3.3
// TestE2E_DriveList_StaleStateDB verifies that `drive list` shows a
// "[has sync data]" marker for drives that were synced and then removed
// from the config (orphaned state DB).
func TestE2E_DriveList_StaleStateDB(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Step 1: Create a file and sync it up to establish a state DB.
	testFolder := fmt.Sprintf("e2e-stale-db-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "stale-test.txt"),
		[]byte("stale db test\n"), 0o600))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runSyncThroughTransientFailures(t, cfgPath, env, "--upload-only")

	// Wait for file to be visible remotely.
	remotePath := "/" + testFolder + "/stale-test.txt"
	waitForRemoteReadContains(t, cfgPath, env, "", "stale-test.txt", pollTimeout, "stat", remotePath)

	// Step 2: Remove the drive from config (preserves state DB on disk).
	stdout, _, err := runCLICore(t, cfgPath, env, drive, "drive", "remove")
	require.NoError(t, err,
		"drive remove should succeed\nstdout: %s", printable(stdout))

	// Step 3: Verify text output shows [has sync data] for the now-available drive.
	stdout, _, err = runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err,
		"drive list should succeed after drive remove\nstdout: %s", printable(stdout))

	assertOutputContains(t, stdout, "Available drives",
		"removed drive should appear in available drives section")
	assertOutputContains(t, stdout, "[has sync data]",
		"removed drive with state DB should show [has sync data] marker")

	// Step 4: Verify JSON output includes has_state_db: true.
	jsonOut, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err,
		"drive list --json should succeed\nstdout: %s", printable(jsonOut))

	var result struct {
		Available []struct {
			CanonicalID string `json:"canonical_id"`
			HasStateDB  bool   `json:"has_state_db"`
		} `json:"available"`
	}

	require.NoError(t, json.Unmarshal([]byte(jsonOut), &result),
		"JSON should parse, got: %s", printable(jsonOut))

	// Find the drive we just removed — it should have has_state_db: true.
	found := false
	for _, avail := range result.Available {
		if avail.CanonicalID == drive {
			found = true
			assert.True(t, avail.HasStateDB,
				"removed drive %s should have has_state_db=true", suiteDriveRoles.Role(drive))

			break
		}
	}

	assert.True(t, found,
		"removed drive %s should appear in available array", suiteDriveRoles.Role(drive))
}
