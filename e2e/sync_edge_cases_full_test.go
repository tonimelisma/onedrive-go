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
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-emptydir-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder, "emptyFolder")
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Sync upload — folder creation.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify folder exists remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "emptyFolder", pollTimeout, "ls", "/"+testFolder)

	// Advance the delta token past the creation by running a no-op sync.
	// The subsequent deletion must occur AFTER the saved delta token for
	// incremental delta to report it (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Delete folder remotely.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "-r", "/"+testFolder+"/emptyFolder")

	// Wait for deletion to propagate via REST.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "emptyFolder", pollTimeout, "ls", "/"+testFolder)

	// Delta endpoint may lag behind REST item endpoints (ci_issues.md §17).
	// Re-run sync until delta catches up and the deletion propagates locally.
	// Don't use --force: a fresh delta only lists existing items, so the
	// deletion would be invisible. Incremental delta (with saved token) is
	// required. Big-delete protection won't trigger (< 10 baseline items).
	// Use runCLIWithConfigAllowError inside Eventually to prevent panic
	// when the test times out (require.Eventually runs in a goroutine).
	// Delta endpoint may lag 60-120s behind REST; use 180s to avoid flakes.
	require.Eventually(t, func() bool {
		_, _, syncErr := runCLIWithConfigAllowError(t, cfgPath, env, "sync", "--download-only")
		if syncErr != nil {
			return false
		}
		_, statErr := os.Stat(localDir)
		return os.IsNotExist(statErr)
	}, 180*time.Second, 5*time.Second, "empty folder should be deleted locally after remote delete")
}

// TestE2E_Sync_NestedDeletion validates that deleting a deeply nested remote
// folder tree results in the entire local tree being removed.
func TestE2E_Sync_NestedDeletion(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-nestdel-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a/b/c/ tree with files at each level.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "a", "b", "c"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "top.txt"), []byte("top level"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "mid.txt"), []byte("mid level"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "c", "deep.txt"), []byte("deep level"), 0o600))

	// Sync upload.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify deep file exists remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "deep.txt", pollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Advance delta token past the creation (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Delete entire tree remotely.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "-r", "/"+testFolder+"/a")

	// Wait for deletion to propagate via REST.
	pollCLIWithConfigNotContains(t, opsCfgPath, nil, "a", pollTimeout, "ls", "/"+testFolder)

	// Delta endpoint may lag behind REST (ci_issues.md §17). Re-sync until
	// delta catches up and the tree deletion propagates locally.
	// Use runCLIWithConfigAllowError inside Eventually to prevent panic
	// when the test times out (require.Eventually runs in a goroutine).
	// With cascade expansion (sync-planning.md §Folder Delete Cascade),
	// once delta reports the parent folder deletion, all children are
	// deleted in a single pass — no need for multiple delta cycles.
	require.Eventually(t, func() bool {
		_, _, syncErr := runCLIWithConfigAllowError(t, cfgPath, env, "sync", "--download-only")
		if syncErr != nil {
			return false
		}
		_, statErr := os.Stat(filepath.Join(localDir, "a"))
		return os.IsNotExist(statErr)
	}, 120*time.Second, 5*time.Second, "a/ directory should be deleted locally after remote delete")

	// Verify entire local tree is gone.
	_, err := os.Stat(filepath.Join(localDir, "a", "b", "c", "deep.txt"))
	assert.True(t, os.IsNotExist(err), "deep.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "b", "mid.txt"))
	assert.True(t, os.IsNotExist(err), "mid.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "top.txt"))
	assert.True(t, os.IsNotExist(err), "top.txt should be deleted")
}

// TestE2E_Sync_ResolveKeepLocalThenSync resolves an edit-edit conflict with
// --keep-local, then syncs to verify the remote gets the local content.
func TestE2E_Sync_ResolveKeepLocalThenSync(t *testing.T) {
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-reskl-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "keeplocal.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Modify both sides to create edit-edit conflict.
	require.NoError(t, os.WriteFile(filePath, []byte("local version wins"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/keeplocal.txt", "remote version loses")

	// Bidirectional sync — detects conflict.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve --keep-local.
	runCLIWithConfig(t, cfgPath, env, "conflicts", "resolve", testFolder+"/keeplocal.txt", "--keep-local")

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
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-reskr-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file and upload baseline.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "keepremote.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Modify both sides.
	require.NoError(t, os.WriteFile(filePath, []byte("local version loses"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/keepremote.txt", "remote version wins")

	// Bidirectional sync — conflict detected, remote content downloaded.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Resolve --keep-remote.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "conflicts", "resolve", testFolder+"/keepremote.txt", "--keep-remote")
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
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, ".nosync"), []byte{}, 0o600))

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
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "mtime-only.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("stable content"), 0o600))

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
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-idemp-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create files and a subfolder.
	localDir := filepath.Join(syncDir, testFolder)
	subDir := filepath.Join(localDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("file a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("file b"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("file c"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested content"), 0o600))

	// Sync bidirectional.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Re-sync — the owned subtree should stay stable even if unrelated tests
	// churn elsewhere on the shared live drive.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync", "--force")

	// The test-owned remote subtree should still contain the same files.
	stdout, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "a.txt")
	assert.Contains(t, stdout, "b.txt")
	assert.Contains(t, stdout, "c.txt")
	assert.Contains(t, stdout, "sub")

	subOut, _ := runCLIWithConfig(t, opsCfgPath, nil, "ls", "/"+testFolder+"/sub")
	assert.Contains(t, subOut, "nested.txt")
}

// TestE2E_Sync_TransferWorkersConfig validates that the transfer_workers
// config option is respected (sync completes with non-default worker count).
func TestE2E_Sync_TransferWorkersConfig(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "transfer_workers = 4\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-workers-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 5 files.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("worker-file-%d.txt", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, name),
			[]byte(fmt.Sprintf("content %d", i)),
			0o644,
		))
	}

	// Sync upload.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify all 5 files exist remotely.
	stdout, _ := pollCLIWithConfigContains(t, opsCfgPath, nil, "worker-file-5.txt", pollTimeout, "ls", "/"+testFolder)
	for i := 1; i <= 5; i++ {
		assert.Contains(t, stdout, fmt.Sprintf("worker-file-%d.txt", i))
	}
}
