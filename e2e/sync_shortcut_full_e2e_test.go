//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// shortcutWriteStep names a test's write into a shortcut's local folder in
// its failure message.
const shortcutWriteStep = "write into the shortcut"

// Validates: R-2.8.1
func TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureReadOnly)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	record, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)
	assertLocalPath(t, filepath.Join(syncDir, fixture.ShortcutName), localFolder)
	assertLocalPath(t, filepath.Join(syncDir, fixture.ShortcutName, filepath.FromSlash(fixture.SentinelPath)), localFile)

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
	require.NoErrorf(t, json.Unmarshal([]byte(stdout), &drives),
		"drive list --json output should be valid JSON (%d bytes, in the test's debug log)", len(stdout))

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

	firstRecord, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)
	// Single pass by design: one restart pass must keep the same child mount
	// without a re-projection; it may leave work a transient failure causes.
	runSyncPassForDrive(t, cfgPath, env, fixture.ParentDrive, "--download-only")
	secondRecord := requireActiveShortcutChild(t, env, fixture)

	assertSameIdentity(t, "the child mount ID after the change", firstRecord.MountID, secondRecord.MountID)
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

	firstRecord, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)
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

	secondRecord, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, newName)

	assertSameIdentity(t, "the child mount ID after the change", firstRecord.MountID, secondRecord.MountID)
	assertSamePath(t, "the child mount's state database", firstStatePath, e2eMountStatePath(env, secondRecord.MountID))
	assertLocalPath(t, filepath.Join(syncDir, fixture.ShortcutName), localNoFolder)
	assertLocalPath(t, filepath.Join(syncDir, newName), localFolder)
	assertLocalPath(t, filepath.Join(syncDir, newName, filepath.FromSlash(fixture.SentinelPath)), localFile)

	status := readStatusAllDrives(t, cfgPath, env)
	parent := requireStatusDrive(t, status, fixture.ParentDrive)
	child, ok := findStatusSharedFolderJSON(parent, secondRecord.MountID)
	require.True(t, ok)
	assert.Contains(t, child.Folder, newName)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_MoveReusesChildMountState(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixture(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	firstRecord, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)
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
		runCLIBestEffort(t, cfgPath, env, fixture.ParentDrive, "rm", "-r", containerRemotePath)
	})

	mkdirRemoteFolderForDrive(t, cfgPath, env, fixture.ParentDrive, containerRemotePath)
	runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "mv", originalRemotePath, containerRemotePath)
	currentRemotePath = movedRemotePath

	secondRecord, _ := syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, movedRelativePath)

	assertSameIdentity(t, "the child mount ID after the change", firstRecord.MountID, secondRecord.MountID)
	assertSamePath(t, "the child mount's state database", firstStatePath, e2eMountStatePath(env, secondRecord.MountID))
	assertLocalPath(t, filepath.Join(syncDir, fixture.ShortcutName), localNoFolder)
	assertLocalPath(t, filepath.Join(syncDir, filepath.FromSlash(movedRelativePath)), localFolder)
	assertLocalPath(t, filepath.Join(syncDir, filepath.FromSlash(movedRelativePath), filepath.FromSlash(fixture.SentinelPath)), localFile)

	status := readStatusAllDrives(t, cfgPath, env)
	parent := requireStatusDrive(t, status, fixture.ParentDrive)
	child, ok := findStatusSharedFolderJSON(parent, secondRecord.MountID)
	require.True(t, ok)
	assertOutputContains(t, child.Folder, filepath.FromSlash(movedRelativePath))
}

// Validates: R-2.8.1
func TestE2E_Shortcut_WritableUploadSyncsToSharedTarget(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)

	remoteName := fmt.Sprintf("shortcut-upload-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("shortcut child upload %d\n", time.Now().UnixNano())
	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	requireNoLocalError(t, os.WriteFile(localPath, []byte(expectedContent), 0o600), shortcutWriteStep)

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	t.Cleanup(func() {
		runCLIBestEffort(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	out, _ := runSyncThroughTransientFailuresForDrive(t, cfgPath, env, fixture.ParentDrive, "--upload-only")
	assertOutputContains(t, out, "(upload only)")

	waitForRemoteReadContains(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, remoteName, pollTimeout, "stat", remotePath)
}

// Validates: R-2.8.1, R-6.6.28
func TestE2E_Shortcut_ReadOnlyBlockedUploadStatus(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureReadOnly)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)

	blockedName := fmt.Sprintf("blocked-shortcut-%d.txt", time.Now().UnixNano())
	blockedLocalPath := filepath.Join(syncDir, fixture.ShortcutName, blockedName)
	requireNoLocalError(t, os.WriteFile(blockedLocalPath, []byte("recipient write attempt\n"), 0o600), shortcutWriteStep)

	// Single pass by design: this one pass must record the denied write that
	// status shows next. The file stays unsynced, so the pass exits 1.
	out, stderr, err := runCLIWithConfigAllowErrorForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	assertOutputContains(t, out, "(upload only)")
	requireSyncLeftWork(t, out, stderr, err)

	statusOut, _ := runCLIWithConfigForDrive(t, cfgPath, env, fixture.ParentDrive, "status")
	assertOutputContains(t, statusOut, "Shared folder writes blocked")
	assertOutputContains(t, statusOut, "Downloads continue normally.")
	assertOutputContains(t, statusOut, blockedName)

	// Single pass by design: a later run does not finish the write either,
	// and says so with its exit status, as status does (R-6.6.28).
	out, stderr, err = runCLIWithConfigAllowErrorForDrive(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	requireSyncLeftWork(t, out, stderr, err)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	childRoot := filepath.Join(syncDir, fixture.ShortcutName)
	requireNoLocalError(t, os.WriteFile(childRoot, []byte("not a directory\n"), 0o600), "write a file where the shortcut goes")

	parentName := fmt.Sprintf("shortcut-parent-survives-%d.txt", time.Now().UnixNano())
	parentRemotePath := "/" + parentName
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, parentName), []byte("parent still syncs\n"), 0o600))
	t.Cleanup(func() {
		runCLIBestEffort(t, cfgPath, env, fixture.ParentDrive, "rm", parentRemotePath)
	})

	out, stderr, err := runCLICore(t, cfgPath, env, fixture.ParentDrive, "sync", "--upload-only")
	require.Error(t, err)
	assertOutputContains(t, out, "(upload only)")
	assertOutputContains(t, stderr, string(syncshortcut.ShortcutRootStateBlockedPath))
	assert.Equal(t, 1, strings.Count(out, "Uploaded "), "exactly one upload should succeed")

	record := requireShortcutChildAtPath(t, env, fixture, fixture.ShortcutName)
	assert.Equal(t, syncshortcut.ShortcutRootStateBlockedPath, record.State)
	assert.Equal(t, string(syncshortcut.ShortcutRootStateBlockedPath), record.StateReason)
	waitForRemoteReadContains(t, cfgPath, env, fixture.ParentDrive, parentName, pollTimeout, "stat", parentRemotePath)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	remoteName := fmt.Sprintf("shortcut-watch-local-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	t.Cleanup(func() {
		runCLIBestEffort(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	eventsPath := withShortcutWatchDaemon(t, cfgPath, env, fixture.ParentDrive, "--upload-only")
	waitForDaemonReady(t, eventsPath)

	const uploadContent = "watch local shortcut upload\n"
	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	requireNoLocalError(t, os.WriteFile(localPath, []byte(uploadContent), 0o600), shortcutWriteStep)

	// Wait for the full size, not only the name: the daemon stops when the
	// test ends, and stopping it between the create and the content leaves
	// an empty file in the shared folder (LI-20261110-01).
	waitForRemoteStatSizeForDrive(
		t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, remotePath, int64(len(uploadContent)), 3*time.Minute)
}

// Validates: R-2.8.1
func TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot(t *testing.T) {
	registerLogDump(t)

	fixture := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, syncDir)

	syncShortcutDownloadUntilProjected(t, cfgPath, env, fixture, syncDir, fixture.ShortcutName)

	opsCfgPath, opsEnv := writeShortcutSharedDriveConfig(t, fixture)
	remoteName := fmt.Sprintf("shortcut-watch-remote-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("watch remote shortcut wake %d\n", time.Now().UnixNano())
	contentFile := writeTempContentFile(t, expectedContent)
	t.Cleanup(func() {
		runCLIBestEffort(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "rm", remotePath)
	})

	eventsPath := withShortcutWatchDaemon(t, cfgPath, env, fixture.ParentDrive, "--download-only")
	waitForDaemonReady(t, eventsPath)

	runCLIWithConfigForDrive(t, opsCfgPath, opsEnv, fixture.SharedItem.Selector, "put", contentFile, remotePath)

	localPath := filepath.Join(syncDir, fixture.ShortcutName, remoteName)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(localPath) //nolint:gosec // a file inside the test's own sync dir
		return err == nil && string(data) == expectedContent
	}, 3*time.Minute, 2*time.Second)
}

func writeShortcutSharedDriveConfig(t *testing.T, fixture *resolvedShortcutFixture) (string, map[string]string) {
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
) string {
	t.Helper()

	daemonArgs := []string{
		"--config", cfgPath,
		"--drive", driveID,
		"--debug",
		"sync", "--watch", modeArg,
	}
	env, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, env)

	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start shortcut watch daemon")
	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	return eventsPath
}

func requireShortcutChildAtPath(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	relativeLocalPath string,
) shortcutChildE2ERecord {
	t.Helper()

	record, ok := findShortcutChildAtPath(t, env, fixture, relativeLocalPath)
	if ok {
		return record
	}

	require.Failf(t,
		"shortcut child mount missing at projection path",
		"parent=%s shortcut=%q relative_path=%q remote=fixture.SharedItem",
		suiteDriveRoles.Role(fixture.ParentDrive),
		suiteDriveRoles.Redact(fixture.ShortcutName),
		suiteDriveRoles.Redact(relativeLocalPath),
	)
	return shortcutChildE2ERecord{}
}

func findShortcutChildAtPath(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	relativeLocalPath string,
) (shortcutChildE2ERecord, bool) {
	t.Helper()

	for _, record := range shortcutChildRecords(t, env, fixture) {
		if record.NamespaceID != fixture.ParentDrive ||
			record.RelativeLocalPath != relativeLocalPath {
			continue
		}
		return record, true
	}

	return shortcutChildE2ERecord{}, false
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
			suiteDriveRoles.Redact(*currentPath), suiteDriveRoles.Redact(originalPath), err,
			printable(stdout), printable(stderr))
		return
	}
	*currentPath = originalPath
	parentPath := path.Dir(originalPath)
	if parentPath == "." {
		parentPath = "/"
	}
	waitForRemoteReadContains(
		t,
		cfgPath,
		env,
		driveID,
		path.Base(originalPath),
		remoteScopeTransitionTimeout,
		"ls",
		parentPath,
	)
}

func e2eMountStatePath(env map[string]string, mountID string) string {
	statePath := config.MountStatePath(mountID)
	if statePath == "" {
		return ""
	}

	return filepath.Join(env["XDG_DATA_HOME"], "onedrive-go", filepath.Base(statePath))
}
