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

	// Default listing (capped SharePoint discovery).
	defaultOut, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err, "drive list --json should succeed\nstdout: %s", defaultOut)

	var defaultResult struct {
		Available []json.RawMessage `json:"available"`
	}

	require.NoError(t, json.Unmarshal([]byte(defaultOut), &defaultResult),
		"default JSON should parse, got: %s", defaultOut)

	// --all listing (uncapped SharePoint discovery).
	allOut, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json", "--all")
	require.NoError(t, err, "drive list --json --all should succeed\nstdout: %s", allOut)

	var allResult struct {
		Available []json.RawMessage `json:"available"`
	}

	require.NoError(t, json.Unmarshal([]byte(allOut), &allResult),
		"--all JSON should parse, got: %s", allOut)

	// --all should discover at least as many available drives as the default.
	assert.GreaterOrEqual(t, len(allResult.Available), len(defaultResult.Available),
		"--all should discover >= default available drives (default=%d, all=%d)",
		len(defaultResult.Available), len(allResult.Available))
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
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "stale-test.txt"),
		[]byte("stale db test\n"), 0o644))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Wait for file to be visible remotely.
	remotePath := "/" + testFolder + "/stale-test.txt"
	pollCLIWithConfigContains(t, cfgPath, env, "stale-test.txt", pollTimeout, "stat", remotePath)

	// Step 2: Remove the drive from config (preserves state DB on disk).
	stdout, _, err := runCLICore(t, cfgPath, env, drive, "drive", "remove")
	require.NoError(t, err,
		"drive remove should succeed\nstdout: %s", stdout)

	// Step 3: Verify text output shows [has sync data] for the now-available drive.
	stdout, _, err = runCLICore(t, cfgPath, env, "", "drive", "list")
	require.NoError(t, err,
		"drive list should succeed after drive remove\nstdout: %s", stdout)

	assert.Contains(t, stdout, "Available drives",
		"removed drive should appear in available drives section")
	assert.Contains(t, stdout, "[has sync data]",
		"removed drive with state DB should show [has sync data] marker")

	// Step 4: Verify JSON output includes has_state_db: true.
	jsonOut, _, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoError(t, err,
		"drive list --json should succeed\nstdout: %s", jsonOut)

	var result struct {
		Available []struct {
			CanonicalID string `json:"canonical_id"`
			HasStateDB  bool   `json:"has_state_db"`
		} `json:"available"`
	}

	require.NoError(t, json.Unmarshal([]byte(jsonOut), &result),
		"JSON should parse, got: %s", jsonOut)

	// Find the drive we just removed — it should have has_state_db: true.
	found := false
	for _, avail := range result.Available {
		if avail.CanonicalID == drive {
			found = true
			assert.True(t, avail.HasStateDB,
				"removed drive %s should have has_state_db=true", drive)

			break
		}
	}

	assert.True(t, found,
		"removed drive %s should appear in available array", drive)
}
