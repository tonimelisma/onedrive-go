//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.1
func TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureReadOnly)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")

	record := requireActiveShortcutChild(t, env, fixture)
	assert.DirExists(t, filepath.Join(syncDir, fixture.ShortcutName))
	assert.FileExists(t, filepath.Join(syncDir, fixture.ShortcutName, filepath.FromSlash(fixture.SentinelPath)))

	status := readStatusAllDrives(t, cfgPath, env)
	requireStatusDrive(t, status, fixture.ParentDrive)
	requireStatusDrive(t, status, record.MountID)
}

// Validates: R-2.8.1, R-3.6.1
func TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive(t *testing.T) {
	registerLogDump(t)

	fixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", fixture.FolderItem.Selector)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var drives driveListE2EOutput
	require.NoErrorf(t, json.Unmarshal([]byte(stdout), &drives), "drive list --json output should be valid JSON, got: %s", stdout)

	found := false
	for i := range drives.Configured {
		if drives.Configured[i].CanonicalID == fixture.FolderItem.Selector {
			found = true
			break
		}
	}
	assert.True(t, found, "explicit shared folder should remain a configured shared drive")
}

// Validates: R-2.8.1
func TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	firstRecord := requireActiveShortcutChild(t, env, fixture)
	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	secondRecord := requireActiveShortcutChild(t, env, fixture)

	assert.Equal(t, firstRecord.MountID, secondRecord.MountID)
	status := readStatusAllDrives(t, cfgPath, env)
	requireStatusDrive(t, status, fixture.ParentDrive)
	requireStatusDrive(t, status, secondRecord.MountID)
}
