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

func TestE2E_Sync_DryRun(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-dry-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "dryrun.txt"), []byte("dry run test\n"), 0o600))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only", "--dry-run")
	assert.Contains(t, stderr, "Dry run")

	opsCfgPath := writeMinimalConfig(t)
	output := runCLIWithConfigExpectError(t, opsCfgPath, nil, "ls", "/"+testFolder)
	assert.Contains(t, output, testFolder)
}

func TestE2E_Sync_InternalBaselineVerification(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-sync-ver-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "verify-me.txt"), []byte("verify test\n"), 0o600))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	report, err := verifyBaselineReport(t, cfgPath, env)
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.GreaterOrEqual(t, report.Verified, 1)
	assert.Empty(t, report.Mismatches)
}

func TestE2E_Sync_Conflicts(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-fast-conflict-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	conflictFile := filepath.Join(localDir, "conflict.txt")
	require.NoError(t, os.WriteFile(conflictFile, []byte("original"), 0o600))

	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")

	require.NoError(t, os.WriteFile(conflictFile, []byte("local edit"), 0o600))
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/conflict.txt", "remote edit")
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync")
	assert.Contains(t, stderr, "Conflicts:")

	stdout, _ := runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "conflict.txt")

	queueConflictResolutionAndSync(t, cfgPath, env, "remote", testFolder+"/conflict.txt")

	stdout, _ = runCLIWithConfig(t, cfgPath, env, "status")
	assert.Contains(t, stdout, "No unresolved conflicts.")

	content, err := os.ReadFile(conflictFile)
	require.NoError(t, err)
	assert.Equal(t, "remote edit", string(content))
}

func TestE2E_Sync_DriveRemoveAndReAdd(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	tempDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(tempDataDir, 0o700))
	copyTokenFile(t, testDataDir, tempDataDir)

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	testFolder := fmt.Sprintf("e2e-sync-readd-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	writeConfig := func(t *testing.T) string {
		t.Helper()

		content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, drive, syncDir)
		cfgPath := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

		return cfgPath
	}

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file1.txt"), []byte("first file\n"), 0o644))

	cfgPath := writeConfig(t)
	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	firstRelPath := filepath.ToSlash(filepath.Join(testFolder, "file1.txt"))
	requireBaselinePathPresent(t, env, firstRelPath)

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, []byte(""), 0o600))

	cfgPath2 := writeConfig(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "file2.txt"), []byte("second file\n"), 0o644))

	_, stderr = runCLIWithConfig(t, cfgPath2, env, "sync", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	secondRelPath := filepath.ToSlash(filepath.Join(testFolder, "file2.txt"))
	requireBaselinePathPresent(t, env, firstRelPath)
	requireBaselinePathPresent(t, env, secondRelPath)
}
