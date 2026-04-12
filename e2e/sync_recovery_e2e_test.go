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
// Validates: R-2.15.1
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	// Re-sync should show no changes (delta token persisted).
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	assert.Contains(t, stderr, "No changes detected",
		"incremental sync should detect no changes after all files uploaded")

	// Add a new file remotely.
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/delta-new.txt", "new from remote")

	// Download-only sync — should pick up only the new file.
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
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
// Validates: R-2.5.1
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

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
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Re-sync immediately — should show no changes (idempotent).
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")
	assert.Contains(t, stderr, "No changes detected",
		"immediate re-sync after mixed changes should be idempotent")
}

// Validates: R-2.5.1
func TestE2E_Sync_ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta(t *testing.T) {
	// No t.Parallel() — this test advances the live delta token in a
	// directional run, then verifies a later run settles remote drift from the
	// durable mirror even when there are no new delta events left to consume.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-remote-mirror-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	remoteEditPath := filepath.Join(localDir, "remote-edit.txt")
	remoteDeletePath := filepath.Join(localDir, "remote-delete.txt")
	require.NoError(t, os.WriteFile(remoteEditPath, []byte("original remote edit"), 0o600))
	require.NoError(t, os.WriteFile(remoteDeletePath, []byte("original remote delete"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	pollCLIWithConfigContains(t, opsCfgPath, nil, "remote-delete.txt", pollTimeout, "ls", "/"+testFolder)

	// Establish a real delta token plus remote mirror rows for this live drive
	// before creating fresh remote drift.
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/remote-edit.txt", "updated from remote")
	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/remote-delete.txt")

	// Upload-only should still observe remote truth and advance the delta token,
	// but it must not settle the download-only side effects yet.
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	assert.NotContains(t, stderr, "No changes detected",
		"upload-only should observe remote drift even when it cannot apply it")

	editData, err := os.ReadFile(remoteEditPath)
	require.NoError(t, err)
	assert.Equal(t, "original remote edit", string(editData))

	_, err = os.Stat(remoteDeletePath)
	require.NoError(t, err)

	// The next download-only run should settle the already-observed remote
	// drift from durable mirror truth, even without fresh delta events.
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.NotContains(t, stderr, "No changes detected",
		"download-only should settle durable remote mirror drift")

	editData, err = os.ReadFile(remoteEditPath)
	require.NoError(t, err)
	assert.Equal(t, "updated from remote", string(editData))

	_, err = os.Stat(remoteDeletePath)
	assert.ErrorIs(t, err, os.ErrNotExist)

	snapshot := snapshotLocalTree(t, localDir)
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.Contains(t, stderr, "No changes detected",
		"immediate rerun after durable mirror settlement should be idle")
	assert.Equal(t, snapshot, snapshotLocalTree(t, localDir))
}

// TestE2E_Sync_DriveRemovePurgeResetsState validates that deleting the state
// DB forces a full re-enumeration on the next sync.
// Validates: R-2.5.1
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

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

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
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
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
