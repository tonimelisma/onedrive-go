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
// Sync edge case E2E tests (slow — run only with -tags=e2e,e2e_full)
// ---------------------------------------------------------------------------

// TestE2E_Sync_EmptyDirectory validates that empty local folders are created
// remotely and that remote folder deletion propagates locally.
func TestE2E_Sync_EmptyDirectory(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-emptydir-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder, "emptyFolder")
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	// Sync upload — folder creation.
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Verify folder exists remotely.
	waitForRemoteReadContains(t, cfgPath, env, "", "emptyFolder", pollTimeout, "ls", "/"+testFolder)

	// Advance the delta token past the creation by running a no-op sync.
	// The subsequent deletion must occur AFTER the saved delta token for
	// incremental delta to report it (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Delete folder remotely.
	runCLIWithConfig(t, cfgPath, env, "rm", "-r", "/"+testFolder+"/emptyFolder")

	// Wait for deletion to propagate via REST.
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "emptyFolder", "ls", "/"+testFolder)

	// Delta endpoint may lag behind REST item endpoints (ci_issues.md §17).
	// Re-run sync until delta catches up and the deletion propagates locally.
	// Incremental delta (with saved token) is required because a fresh delta
	// only lists existing items, so the deletion would be invisible.
	// Delete safety protection won't trigger (< 10 baseline items).
	// Delta endpoint may lag 60-120s behind REST; use 180s to avoid flakes.
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		180*time.Second,
		"empty folder should be deleted locally after remote delete",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			_, statErr := os.Stat(localDir)
			return os.IsNotExist(statErr)
		},
		"--download-only",
	)
}

// TestE2E_Sync_NestedDeletion validates that deleting a deeply nested remote
// folder tree results in the entire local tree being removed.
func TestE2E_Sync_NestedDeletion(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-nestdel-%d", time.Now().UnixNano())

	// Create a/b/c/ tree with files at each level.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "a", "b", "c"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "top.txt"), []byte("top level"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "mid.txt"), []byte("mid level"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a", "b", "c", "deep.txt"), []byte("deep level"), 0o600))

	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		transientGraphRetryTimeout,
		"nested deletion setup should upload local tree despite transient Graph gateway failures",
		func(result syncAttemptResult) bool {
			if result.Err == nil {
				return true
			}
			require.Truef(t, isRetryableGraphGatewayFailure(result.Stderr),
				"unexpected sync setup failure\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
			return false
		},
		"--upload-only",
	)

	// Verify deep file exists remotely.
	waitForRemoteReadContains(t, cfgPath, env, "", "deep.txt", pollTimeout, "ls", "/"+testFolder+"/a/b/c")

	// Advance delta token past the creation (ci_issues.md §17).
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	// Delete entire tree remotely.
	runCLIWithConfig(t, cfgPath, env, "rm", "-r", "/"+testFolder+"/a")

	// Wait for deletion to propagate via REST.
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "a", "ls", "/"+testFolder)

	// Delta endpoint may lag behind REST (ci_issues.md §17). Re-sync until
	// delta catches up and the tree deletion propagates locally.
	// With cascade expansion (sync-planning.md §Folder Delete Cascade),
	// once delta reports the parent folder deletion, all children are
	// deleted in a single pass — no need for multiple delta cycles.
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"a/ directory should be deleted locally after remote delete",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			_, statErr := os.Stat(filepath.Join(localDir, "a"))
			return os.IsNotExist(statErr)
		},
		"--download-only",
	)

	// Verify entire local tree is gone.
	_, err := os.Stat(filepath.Join(localDir, "a", "b", "c", "deep.txt"))
	assert.True(t, os.IsNotExist(err), "deep.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "b", "mid.txt"))
	assert.True(t, os.IsNotExist(err), "mid.txt should be deleted")

	_, err = os.Stat(filepath.Join(localDir, "a", "top.txt"))
	assert.True(t, os.IsNotExist(err), "top.txt should be deleted")
}

// TestE2E_Sync_MtimeOnlyChange validates that changing only mtime (without
// content change) does not trigger a re-upload. The scanner compares hashes
// against baseline and discards events where hashes match.
func TestE2E_Sync_MtimeOnlyChange(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-mtime-%d", time.Now().UnixNano())

	// Create file and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	filePath := filepath.Join(localDir, "mtime-only.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("stable content"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Change only mtime (content stays the same).
	newTime := time.Now().Add(-24 * time.Hour)
	require.NoError(t, os.Chtimes(filePath, newTime, newTime))

	// Re-sync — should not schedule uploads for the owned subtree when only
	// mtime changed and content/hash stayed the same. Shared-drive delta churn
	// elsewhere can still make the global run report unrelated deferred work.
	stderr := assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync", "--upload-only")
	assert.NotContains(t, stderr, "Uploads:",
		"mtime-only change should not trigger upload when hash matches baseline")
}

// TestE2E_Sync_IdempotentReSync validates that re-syncing immediately after
// a successful sync reports no changes.
func TestE2E_Sync_IdempotentReSync(t *testing.T) {
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-idemp-%d", time.Now().UnixNano())

	// Create files and a subfolder.
	localDir := filepath.Join(syncDir, testFolder)
	subDir := filepath.Join(localDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("file a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "b.txt"), []byte("file b"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "c.txt"), []byte("file c"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested content"), 0o600))

	// Sync bidirectional.
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Re-sync — the owned subtree should stay stable even if unrelated tests
	// churn elsewhere on the shared live drive.
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, localDir, "sync")

	// The test-owned remote subtree should still contain the same files.
	stdout, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "a.txt")
	assert.Contains(t, stdout, "b.txt")
	assert.Contains(t, stdout, "c.txt")
	assert.Contains(t, stdout, "sub")

	subOut, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder+"/sub")
	assert.Contains(t, subOut, "nested.txt")
}

// TestE2E_Sync_TransferWorkersConfig validates that the transfer_workers
// config option is respected without assuming every concurrent upload must
// complete in one pass through retryable transient Graph service outages.
func TestE2E_Sync_TransferWorkersConfig(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfigWithOptions(t, syncDir, "transfer_workers = 4\n")

	testFolder := fmt.Sprintf("e2e-sync-workers-%d", time.Now().UnixNano())

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

	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		120*time.Second,
		"upload-only should eventually publish all worker test files with non-default transfer_workers",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			stdout, _ := runCLIWithConfig(t, cfgPath, env, "ls", "/"+testFolder)
			for i := 1; i <= 5; i++ {
				if !strings.Contains(stdout, fmt.Sprintf("worker-file-%d.txt", i)) {
					return false
				}
			}

			return true
		},
		"--upload-only",
	)
	assert.Contains(t, attempt.Stderr, "Mode: upload-only")
}
