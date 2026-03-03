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
// Sync edge case E2E tests (slow — run only with -tags=e2e,e2e_full)
// ---------------------------------------------------------------------------

// TestE2E_Sync_EmptyDirectory validates that empty local folders are created
// remotely and that remote folder deletion propagates locally.
func TestE2E_Sync_EmptyDirectory(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-emptydir-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder, "emptyFolder")
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Sync upload — folder creation.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify folder exists remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "emptyFolder", pollTimeout, "ls", "/"+testFolder)

	// Delete folder remotely.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "-r", "/"+testFolder+"/emptyFolder")

	// Wait for deletion to propagate.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "emptyFolder", pollTimeout, "ls", "/"+testFolder)

	// Sync download — folder deletion should propagate locally.
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only", "--force")

	// Verify local folder is gone.
	_, err := os.Stat(localDir)
	assert.True(t, os.IsNotExist(err), "empty folder should be deleted locally after remote delete")
}

// TestE2E_Sync_NestedDeletion validates that deleting a deeply nested remote
// folder tree results in the entire local tree being removed.
func TestE2E_Sync_NestedDeletion(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-nestdel-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a/b/c/ tree with files at each level.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "a", "b", "c"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "top.txt"), []byte("top level"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "mid.txt"), []byte("mid level"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "c", "deep.txt"), []byte("deep level"), 0o644))

	// Sync upload (two syncs for folder creation + file upload).
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify deep file exists remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "deep.txt", pollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Delete entire tree remotely.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "-r", "/"+testFolder+"/a")

	// Wait for deletion to propagate.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "a", pollTimeout, "ls", "/"+testFolder)

	// Sync download — entire tree should be deleted locally.
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only", "--force")

	// Verify local tree is gone (children before parents ordering).
	_, err := os.Stat(filepath.Join(localDir, "a", "b", "c", "deep.txt"))
	assert.True(t, os.IsNotExist(err), "deep.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "b", "mid.txt"))
	assert.True(t, os.IsNotExist(err), "mid.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "top.txt"))
	assert.True(t, os.IsNotExist(err), "top.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a"))
	assert.True(t, os.IsNotExist(err), "a/ directory should be deleted")
}

// TestE2E_Sync_ResolveKeepLocalThenSync resolves an edit-edit conflict with
// --keep-local, then syncs to verify the remote gets the local content.
func TestE2E_Sync_ResolveKeepLocalThenSync(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-reskl-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	filePath := filepath.Join(localDir, "keeplocal.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Modify both sides to create edit-edit conflict.
	require.NoError(t, os.WriteFile(filePath, []byte("local version wins"), 0o644))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/keeplocal.txt", "remote version loses")

	// Bidirectional sync — detects conflict.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve --keep-local.
	runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/keeplocal.txt", "--keep-local")

	// Sync to push local version to remote.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Verify remote has local content.
	remoteContent := getRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/keeplocal.txt")
	assert.Equal(t, "local version wins", remoteContent)
}

// TestE2E_Sync_ResolveKeepRemoteThenSync resolves an edit-edit conflict with
// --keep-remote, syncs, and verifies local has remote content with conflict
// copy cleaned up.
func TestE2E_Sync_ResolveKeepRemoteThenSync(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-reskr-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	filePath := filepath.Join(localDir, "keepremote.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Modify both sides.
	require.NoError(t, os.WriteFile(filePath, []byte("local version loses"), 0o644))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/keepremote.txt", "remote version wins")

	// Bidirectional sync — conflict detected, remote content downloaded.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve --keep-remote.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "resolve", testFolder+"/keepremote.txt", "--keep-remote")
	assert.Contains(t, stderr, "Resolved")

	// Sync to finalize.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Verify local file has remote content.
	localData, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "remote version wins", string(localData))

	// Verify conflict copies are cleaned up.
	matches, err := filepath.Glob(filepath.Join(localDir, "keepremote.conflict-*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "conflict copies should be cleaned up after resolve --keep-remote")
}

// TestE2E_Sync_NosyncGuard validates that a .nosync file in the sync root
// prevents sync from running (S2 safety guard).
func TestE2E_Sync_NosyncGuard(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Create .nosync guard file in sync root.
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, ".nosync"), []byte{}, 0o644))

	// Sync should fail with nosync error.
	output := runCLIWithConfigExpectError(t, cfgPath, env, "sync", "--upload-only", "--force")
	assert.Contains(t, output, "nosync", "sync should report .nosync guard file presence")
}

// TestE2E_Sync_MtimeOnlyChange validates that changing only mtime (without
// content change) does not trigger a re-upload. The scanner compares hashes
// against baseline and discards events where hashes match.
func TestE2E_Sync_MtimeOnlyChange(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-mtime-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	filePath := filepath.Join(localDir, "mtime-only.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("stable content"), 0o644))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Change only mtime (content stays the same).
	newTime := time.Now().Add(-24 * time.Hour)
	require.NoError(t, os.Chtimes(filePath, newTime, newTime))

	// Re-sync — should detect no changes (hash matches baseline).
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "No changes detected",
		"mtime-only change should not trigger upload when hash matches baseline")
}

// TestE2E_Sync_IdempotentReSync validates that re-syncing immediately after
// a successful sync reports no changes.
func TestE2E_Sync_IdempotentReSync(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-idemp-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create files and a subfolder.
	localDir := filepath.Join(syncDir, testFolder)
	subDir := filepath.Join(localDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("file a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("file b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("file c"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested content"), 0o644))

	// Sync bidirectional (two syncs for folder creation + file upload).
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Re-sync — should show no changes.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected",
		"immediate re-sync should detect no changes")
}

// TestE2E_Sync_TransferWorkersConfig validates that the transfer_workers
// config option is respected (sync completes with non-default worker count).
func TestE2E_Sync_TransferWorkersConfig(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "transfer_workers = 2\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-workers-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 5 files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("worker-file-%d.txt", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, name),
			[]byte(fmt.Sprintf("content %d", i)),
			0o644,
		))
	}

	// Sync upload (two syncs for folder creation + file upload).
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify all 5 files exist remotely.
	stdout, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, "worker-file-5.txt", pollTimeout, "ls", "/"+testFolder)
	for i := 1; i <= 5; i++ {
		assert.Contains(t, stdout, fmt.Sprintf("worker-file-%d.txt", i))
	}
}
