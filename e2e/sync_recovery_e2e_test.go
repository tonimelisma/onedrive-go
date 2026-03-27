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
// Sync recovery E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests validate delta token persistence, idempotent crash recovery,
// and state purge reset behavior.
// ---------------------------------------------------------------------------

// TestE2E_Sync_IncrementalDeltaToken validates that delta tokens persist
// across sync runs, enabling incremental sync (only new changes transferred).
func TestE2E_Sync_IncrementalDeltaToken(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-delta-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 3 local files and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("delta-%d.txt", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, name),
			[]byte(fmt.Sprintf("delta content %d", i)),
			0o644,
		))
	}

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Re-sync should show no changes (delta token persisted).
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "No changes detected",
		"incremental sync should detect no changes after all files uploaded")

	// Add a new file remotely.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/delta-new.txt", "new from remote")

	// Download-only sync — should pick up only the new file.
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--download-only", "--force")
	assert.Contains(t, stderr, "Downloads:",
		"incremental download should report the new file")

	// Verify the new file was downloaded locally.
	newFilePath := filepath.Join(localDir, "delta-new.txt")
	data, err := os.ReadFile(newFilePath)
	require.NoError(t, err)
	assert.Equal(t, "new from remote", string(data))
}

// TestE2E_Sync_CrashRecoveryIdempotent validates that after mixed changes
// (remote deletes + local creates) and a sync, an immediate re-sync detects
// no changes — proving crash recovery is idempotent.
func TestE2E_Sync_CrashRecoveryIdempotent(t *testing.T) {
	// No t.Parallel() — bidirectional sync sees drive-level delta feed,
	// so parallel tests inject cross-test events causing spurious failures.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-crash-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create 5 files and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("crash-%d.txt", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(localDir, name),
			[]byte(fmt.Sprintf("crash content %d", i)),
			0o644,
		))
	}

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify all 5 exist remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "crash-5.txt", pollTimeout, "ls", "/"+testFolder)

	// Delete 2 remotely.
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/crash-1.txt")
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/crash-2.txt")

	// Create 1 locally.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "crash-new.txt"),
		[]byte("new local file"),
		0o644,
	))

	// Bidirectional sync — applies deletes + upload.
	runCLIWithConfig(t, cfgPath, env, "sync", "--force")

	// Re-sync immediately — should show no changes (idempotent).
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--force")
	assert.Contains(t, stderr, "No changes detected",
		"immediate re-sync after mixed changes should be idempotent")
}

// TestE2E_Sync_DriveRemovePurgeResetsState validates that deleting the state
// DB forces a full re-enumeration on the next sync.
func TestE2E_Sync_DriveRemovePurgeResetsState(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-purge-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create files and sync.
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "purge-a.txt"), []byte("purge a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "purge-b.txt"), []byte("purge b"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--force")

	// Verify files exist remotely.
	pollCLIWithConfigContains(t, opsCfgPath, nil, "purge-a.txt", pollTimeout, "ls", "/"+testFolder)

	// Delete all state DB files (simulate purge).
	dataDir := filepath.Join(env["XDG_DATA_HOME"], "onedrive-go")
	dbFiles, err := filepath.Glob(filepath.Join(dataDir, "*.db*"))
	require.NoError(t, err)

	for _, f := range dbFiles {
		require.NoError(t, os.Remove(f), "failed to remove state DB file %s", f)
	}

	// Remove local files so download-only sync re-downloads them.
	require.NoError(t, os.RemoveAll(localDir))

	// Sync download-only — should do full re-enumeration and re-download.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--download-only", "--force")
	assert.NotContains(t, stderr, "No changes detected",
		"sync after purge should re-enumerate, not report no changes")

	// Verify files re-downloaded locally.
	dataA, err := os.ReadFile(filepath.Join(localDir, "purge-a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "purge a", string(dataA))

	dataB, err := os.ReadFile(filepath.Join(localDir, "purge-b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "purge b", string(dataB))
}
