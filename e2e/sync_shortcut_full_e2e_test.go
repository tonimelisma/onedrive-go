//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
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

// Validates: R-2.8.1
func TestE2E_Shortcut_RenameReusesChildMountState(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	firstRecord := requireActiveShortcutChild(t, env, fixture)
	firstStatePath := e2eMountStatePath(env, firstRecord.MountID)
	assert.FileExists(t, firstStatePath)

	originalRemotePath := "/" + fixture.ShortcutName
	newName := fmt.Sprintf("shortcut-rename-%d", time.Now().UnixNano())
	newRemotePath := "/" + newName
	currentRemotePath := originalRemotePath
	t.Cleanup(func() {
		restoreRemoteShortcutPath(t, cfgPath, env, fixture.ParentDrive, &currentRemotePath, originalRemotePath)
	})

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "mv", originalRemotePath, newRemotePath)
	currentRemotePath = newRemotePath

	secondRecord := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, newName)

	assert.Equal(t, firstRecord.MountID, secondRecord.MountID)
	assert.Equal(t, firstStatePath, e2eMountStatePath(env, secondRecord.MountID))
	assert.NoDirExists(t, filepath.Join(syncDir, fixture.ShortcutName))
	assert.DirExists(t, filepath.Join(syncDir, newName))
	assert.FileExists(t, filepath.Join(syncDir, newName, filepath.FromSlash(fixture.SentinelPath)))

	status := readStatusAllDrives(t, cfgPath, env)
	parent := requireStatusDrive(t, status, fixture.ParentDrive)
	child, ok := findStatusMountJSON(parent, secondRecord.MountID)
	require.True(t, ok)
	assert.Equal(t, secondRecord.MountID, child.MountID)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_MoveReusesChildMountState(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	firstRecord := requireActiveShortcutChild(t, env, fixture)
	firstStatePath := e2eMountStatePath(env, firstRecord.MountID)
	assert.FileExists(t, firstStatePath)

	containerName := fmt.Sprintf("e2e-shortcut-move-%d", time.Now().UnixNano())
	containerRemotePath := "/" + containerName
	originalRemotePath := "/" + fixture.ShortcutName
	movedRelativePath := path.Join(containerName, fixture.ShortcutName)
	movedRemotePath := "/" + movedRelativePath
	currentRemotePath := originalRemotePath
	t.Cleanup(func() {
		restoreRemoteShortcutPath(t, cfgPath, env, fixture.ParentDrive, &currentRemotePath, originalRemotePath)
		_, _, _ = runCLICore(t, cfgPath, env, fixture.ParentDrive, "rm", "-r", containerRemotePath)
	})

	mkdirRemoteFolderForDrive(t, cfgPath, env, fixture.ParentDrive, containerRemotePath)
	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "mv", originalRemotePath, containerRemotePath)
	currentRemotePath = movedRemotePath

	secondRecord := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, movedRelativePath)

	assert.Equal(t, firstRecord.MountID, secondRecord.MountID)
	assert.Equal(t, firstStatePath, e2eMountStatePath(env, secondRecord.MountID))
	assert.NoDirExists(t, filepath.Join(syncDir, fixture.ShortcutName))
	assert.DirExists(t, filepath.Join(syncDir, filepath.FromSlash(movedRelativePath)))
	assert.FileExists(t, filepath.Join(syncDir, filepath.FromSlash(movedRelativePath), filepath.FromSlash(fixture.SentinelPath)))

	status := readStatusAllDrives(t, cfgPath, env)
	parent := requireStatusDrive(t, status, fixture.ParentDrive)
	child, ok := findStatusMountJSON(parent, secondRecord.MountID)
	require.True(t, ok)
	assert.Equal(t, secondRecord.MountID, child.MountID)
}

func syncShortcutDownloadUntilProjected(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture resolvedShortcutFixture,
	syncDir string,
	relativeLocalPath string,
) config.MountRecord {
	t.Helper()

	deadline := time.Now().Add(remoteScopeTransitionTimeout)
	var lastStdout, lastStderr string
	var lastErr error
	for attempt := 0; ; attempt++ {
		lastStdout, lastStderr, lastErr = runCLICore(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
		if lastErr == nil {
			record, ok := findActiveShortcutChild(t, env, fixture, relativeLocalPath)
			if ok {
				sentinelPath := filepath.Join(syncDir, filepath.FromSlash(relativeLocalPath), filepath.FromSlash(fixture.SentinelPath))
				if _, err := os.Stat(sentinelPath); err == nil {
					return record
				}
			}
		}

		if time.Now().After(deadline) {
			require.NoErrorf(t, lastErr,
				"shortcut projection did not converge for %q\nstdout: %s\nstderr: %s",
				relativeLocalPath,
				lastStdout,
				lastStderr,
			)
			record := requireActiveShortcutChildAtPath(t, env, fixture, relativeLocalPath)
			sentinelPath := filepath.Join(syncDir, filepath.FromSlash(relativeLocalPath), filepath.FromSlash(fixture.SentinelPath))
			require.FileExists(t, sentinelPath, "shortcut projection state exists but sentinel was not projected")
			return record
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func restoreRemoteShortcutPath(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	currentPath *string,
	originalPath string,
) {
	t.Helper()

	if currentPath == nil || *currentPath == "" || *currentPath == originalPath {
		return
	}

	_, stdout, stderr, err := execCLI(cfgPath, env, driveID, "mv", *currentPath, originalPath)
	if err != nil {
		t.Logf("cleanup restoring shortcut %s to %s failed: %v\nstdout: %s\nstderr: %s",
			*currentPath, originalPath, err, stdout, stderr)
		return
	}
	*currentPath = originalPath
}

func e2eMountStatePath(env map[string]string, mountID string) string {
	statePath := config.MountStatePath(mountID)
	if statePath == "" {
		return ""
	}

	return filepath.Join(env["XDG_DATA_HOME"], "onedrive-go", filepath.Base(statePath))
}
