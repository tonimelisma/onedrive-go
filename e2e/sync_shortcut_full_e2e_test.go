//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"syscall"
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

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
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

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
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

// Validates: R-2.8.1
func TestE2E_Shortcut_WritableUploadSyncsToSharedTarget(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	requireActiveShortcutChild(t, env, fixture)

	remoteName := fmt.Sprintf("shortcut-upload-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("shortcut child upload %d\n", time.Now().UnixNano())
	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	require.NoError(t, os.WriteFile(localPath, []byte(expectedContent), 0o600))

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	waitForRemoteReadContains(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, remoteName, pollTimeout, "stat", remotePath)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_ReadOnlyBlockedUploadStatus(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureReadOnly)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	requireActiveShortcutChild(t, env, fixture)

	blockedName := fmt.Sprintf("blocked-shortcut-%d.txt", time.Now().UnixNano())
	blockedLocalPath := filepath.Join(syncDir, fixture.ShortcutName, blockedName)
	require.NoError(t, os.WriteFile(blockedLocalPath, []byte("recipient write attempt\n"), 0o600))

	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	statusOut, _ := runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "status")
	assert.Contains(t, statusOut, "SHARED FOLDER WRITES BLOCKED")
	assert.Contains(t, statusOut, "Downloads continue normally.")
	assert.Contains(t, statusOut, blockedName)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	childRoot := filepath.Join(syncDir, fixture.ShortcutName)
	require.NoError(t, os.WriteFile(childRoot, []byte("not a directory\n"), 0o600))

	parentName := fmt.Sprintf("shortcut-parent-survives-%d.txt", time.Now().UnixNano())
	parentRemotePath := "/" + parentName
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, parentName), []byte("parent still syncs\n"), 0o600))
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, cfgPath, env, fixture.ParentDrive, "rm", parentRemotePath)
	})

	_, stderr, err := runCLICore(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	require.Error(t, err)
	assert.Contains(t, stderr, "Mode: upload-only")
	assert.Contains(t, stderr, config.MountStateReasonLocalRootCollision)
	assert.Contains(t, stderr, "Succeeded: 1")

	record := requireShortcutChildAtPath(t, env, fixture, fixture.ShortcutName)
	assert.Equal(t, config.MountStateConflict, record.State)
	assert.Equal(t, config.MountStateReasonLocalRootCollision, record.StateReason)
	waitForRemoteReadContains(t, cfgPath, env, fixture.ParentDrive, parentName, pollTimeout, "stat", parentRemotePath)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	requireActiveShortcutChild(t, env, fixture)

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	remoteName := fmt.Sprintf("shortcut-watch-local-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	_, stderr := withShortcutWatchDaemon(t, cfgPath, env, fixture.ParentDrive, "--upload-only")
	waitForDaemonReady(t, stderr, 30*time.Second)

	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	require.NoError(t, os.WriteFile(localPath, []byte("watch local shortcut upload\n"), 0o600))

	waitForRemoteReadContains(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, remoteName, 3*time.Minute, "stat", remotePath)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--download-only")
	requireActiveShortcutChild(t, env, fixture)

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	remoteName := fmt.Sprintf("shortcut-watch-remote-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("watch remote shortcut wake %d\n", time.Now().UnixNano())
	contentFile := writeTempContentFile(t, expectedContent)
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	_, stderr := withShortcutWatchDaemon(t, cfgPath, env, fixture.ParentDrive, "--download-only")
	waitForDaemonReady(t, stderr, 30*time.Second)

	runCLIWithConfigForDrive(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "put", contentFile, remotePath)

	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(localPath)
		return err == nil && string(data) == expectedContent
	}, 3*time.Minute, 2*time.Second)
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

func writeShortcutSharedDriveConfig(t *testing.T, fixture resolvedShortcutFixture) (string, map[string]string) {
	t.Helper()

	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, t.TempDir())
	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", fixture.SharedItem.Selector)

	return cfgPath, env
}

func withShortcutWatchDaemon(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	modeArg string,
) (*syncBuffer, *syncBuffer) {
	t.Helper()

	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", driveID,
		"--debug",
		"sync", "--watch", modeArg,
	}
	cmd := makeCmd(daemonArgs, env)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start shortcut watch daemon")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	return &stdout, &stderr
}

func requireShortcutChildAtPath(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
	relativeLocalPath string,
) config.MountRecord {
	t.Helper()

	record, ok := findShortcutChildAtPath(t, env, fixture, relativeLocalPath)
	if ok {
		return record
	}

	require.Failf(t,
		"shortcut child mount missing at projection path",
		"parent=%s shortcut=%q relative_path=%q remote=%s/%s",
		fixture.ParentDrive,
		fixture.ShortcutName,
		relativeLocalPath,
		fixture.SharedItem.RemoteDriveID,
		fixture.SharedItem.RemoteItemID,
	)
	return config.MountRecord{}
}

func findShortcutChildAtPath(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
	relativeLocalPath string,
) (config.MountRecord, bool) {
	t.Helper()

	dataDir := filepath.Join(env["XDG_DATA_HOME"], "onedrive-go")
	inventory, err := config.LoadMountInventoryForDataDir(dataDir)
	require.NoError(t, err)

	for _, record := range inventory.Mounts {
		if record.NamespaceID != fixture.ParentDrive ||
			record.RelativeLocalPath != relativeLocalPath {
			continue
		}
		if fixture.SharedItem.RemoteDriveID != "" && record.RemoteDriveID != fixture.SharedItem.RemoteDriveID {
			continue
		}
		if fixture.SharedItem.RemoteItemID != "" && record.RemoteItemID != fixture.SharedItem.RemoteItemID {
			continue
		}
		return record, true
	}

	return config.MountRecord{}, false
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
