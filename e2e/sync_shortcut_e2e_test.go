//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.1
func TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	record := requireActiveShortcutChild(t, env, fixture)
	childRoot := filepath.Join(syncDir, fixture.ShortcutName)
	assert.DirExists(t, childRoot)
	assert.FileExists(t, filepath.Join(childRoot, filepath.FromSlash(fixture.SentinelPath)))

	status := readStatusAllDrives(t, cfgPath, env)
	requireStatusDrive(t, status, record.MountID)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var drives driveListE2EOutput
	require.NoErrorf(t, json.Unmarshal([]byte(stdout), &drives), "drive list --json output should be valid JSON, got: %s", stdout)
	for i := range drives.Configured {
		assert.False(t, strings.HasPrefix(drives.Configured[i].CanonicalID, "shared:"),
			"automatic shortcut child mounts must not create explicit shared drive config")
	}

	_, err := os.Stat(filepath.Join(env["XDG_DATA_HOME"], "onedrive-go", "mounts.json"))
	require.NoError(t, err)
}
