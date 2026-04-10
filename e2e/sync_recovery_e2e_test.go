//go:build e2e && e2e_full

package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Sync recovery E2E tests (slow — run only with -tags=e2e,e2e_full)
//
// These tests validate delta token persistence, idempotent crash recovery,
// and state purge reset behavior.
// ---------------------------------------------------------------------------

func stateDBPathForEnv(env map[string]string) string {
	dataHome := env["XDG_DATA_HOME"]
	sanitizedDrive := strings.ReplaceAll(drive, ":", "_")
	return filepath.Join(dataHome, "onedrive-go", "state_"+sanitizedDrive+".db")
}

func openStateDB(t *testing.T, env map[string]string) *sql.DB {
	t.Helper()

	dbPath := stateDBPathForEnv(env)
	require.FileExists(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	return db
}

func remoteStateSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(
		t.Context(),
		`SELECT drive_id, item_id, path, sync_status FROM remote_state ORDER BY path, item_id`,
	)
	require.NoError(t, err)
	defer rows.Close()

	var snapshot []string
	for rows.Next() {
		var (
			driveID string
			itemID  string
			path    string
			status  synctypes.SyncStatus
		)

		require.NoError(t, rows.Scan(&driveID, &itemID, &path, &status))
		snapshot = append(snapshot, fmt.Sprintf("%s %s %s %s", driveID, itemID, path, status))
	}
	require.NoError(t, rows.Err())

	return snapshot
}

func readRemoteStateRowByPath(t *testing.T, db *sql.DB, relPath string) *synctypes.RemoteStateRow {
	t.Helper()

	var row synctypes.RemoteStateRow
	err := db.QueryRowContext(
		t.Context(),
		`SELECT drive_id, item_id, path, sync_status FROM remote_state WHERE path = ?`,
		relPath,
	).Scan(&row.DriveID, &row.ItemID, &row.Path, &row.SyncStatus)
	if err == sql.ErrNoRows {
		return nil
	}
	require.NoError(t, err)

	return &row
}

func setRemoteStateStatusByPath(t *testing.T, db *sql.DB, relPath string, status synctypes.SyncStatus) {
	t.Helper()

	row := readRemoteStateRowByPath(t, db, relPath)
	require.NotNilf(t, row, "expected remote_state row for %s; rows=%v", relPath, remoteStateSnapshot(t, db))

	result, err := db.ExecContext(
		t.Context(),
		`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
		status,
		row.DriveID,
		row.ItemID,
	)
	require.NoError(t, err)

	affected, err := result.RowsAffected()
	require.NoError(t, err)
	assert.EqualValues(t, 1, affected, "expected exactly one remote_state row for %s", relPath)
}

func readRemoteStateStatusByPath(t *testing.T, db *sql.DB, relPath string) synctypes.SyncStatus {
	t.Helper()

	row := readRemoteStateRowByPath(t, db, relPath)
	require.NotNilf(t, row, "expected remote_state row for %s; rows=%v", relPath, remoteStateSnapshot(t, db))
	return row.SyncStatus
}

func countSyncFailuresForPaths(t *testing.T, db *sql.DB, relPaths ...string) int {
	t.Helper()

	if len(relPaths) == 0 {
		return 0
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(relPaths)), ",")
	args := make([]any, 0, len(relPaths))
	for _, relPath := range relPaths {
		args = append(args, relPath)
	}

	var count int
	err := db.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM sync_failures WHERE path IN (`+placeholders+`)`,
		args...,
	).Scan(&count)
	require.NoError(t, err)

	return count
}

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

// Validates: R-2.5.1, R-2.5.4
func TestE2E_Sync_CrashRecovery_ReplaysDurableInProgressRows(t *testing.T) {
	// No t.Parallel() — this seeds crash-shaped durable state against the live
	// drive and then verifies the next sync recovers from that persisted truth.
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-sync-crash-db-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))

	resumePath := filepath.Join(localDir, "resume-me.txt")
	deletePath := filepath.Join(localDir, "delete-me.txt")
	require.NoError(t, os.WriteFile(resumePath, []byte("resume content"), 0o600))
	require.NoError(t, os.WriteFile(deletePath, []byte("delete content"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	pollCLIWithConfigContains(t, opsCfgPath, nil, "delete-me.txt", pollTimeout, "ls", "/"+testFolder)

	// Establish a real delta token plus remote_state rows for this live drive
	// before seeding crash-shaped durable residue. Root-scoped live drives may
	// legitimately observe other preserved fixtures here, so this setup pass is
	// about state initialization, not about idleness.
	runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")

	runCLIWithConfig(t, opsCfgPath, nil, "rm", "/"+testFolder+"/delete-me.txt")
	require.NoError(t, os.Remove(resumePath))

	resumeRelPath := filepath.ToSlash(filepath.Join(testFolder, "resume-me.txt"))
	deleteRelPath := filepath.ToSlash(filepath.Join(testFolder, "delete-me.txt"))

	db := openStateDB(t, env)
	setRemoteStateStatusByPath(t, db, resumeRelPath, synctypes.SyncStatusDownloading)
	setRemoteStateStatusByPath(t, db, deleteRelPath, synctypes.SyncStatusDeleting)
	assert.Equal(t, synctypes.SyncStatusDownloading, readRemoteStateStatusByPath(t, db, resumeRelPath))
	assert.Equal(t, synctypes.SyncStatusDeleting, readRemoteStateStatusByPath(t, db, deleteRelPath))
	require.NoError(t, db.Close())

	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.NotContains(t, stderr, "No changes detected",
		"recovery sync should reconcile the seeded in-progress rows")

	resumeData, err := os.ReadFile(resumePath)
	require.NoError(t, err)
	assert.Equal(t, "resume content", string(resumeData))

	_, err = os.Stat(deletePath)
	assert.ErrorIs(t, err, os.ErrNotExist)

	db = openStateDB(t, env)
	assert.Equal(t, synctypes.SyncStatusSynced, readRemoteStateStatusByPath(t, db, resumeRelPath))
	assert.Equal(t, synctypes.SyncStatusDeleted, readRemoteStateStatusByPath(t, db, deleteRelPath))
	assert.Zero(t, countSyncFailuresForPaths(t, db, resumeRelPath, deleteRelPath))
	require.NoError(t, db.Close())

	snapshot := snapshotLocalTree(t, localDir)
	_, stderr = runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.Contains(t, stderr, "No changes detected",
		"immediate rerun after recovery should be idle")
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
