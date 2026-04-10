//go:build e2e

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

// Validates: R-2.4.5
func TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	testFolder := fmt.Sprintf("e2e-sync-scope-file-%d", time.Now().UnixNano())
	selectedRemotePath := "/" + testFolder + "/docs/report.txt"
	ignoredRemotePath := "/" + testFolder + "/docs/other.txt"

	cfgPath, env := writeSyncConfigWithOptions(t, syncDir,
		fmt.Sprintf("sync_paths = [%q]\n", selectedRemotePath),
	)
	opsCfgPath := writeMinimalConfig(t)

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder+"/docs")
	putRemoteFile(t, opsCfgPath, nil, selectedRemotePath, "selected\n")
	putRemoteFile(t, opsCfgPath, nil, ignoredRemotePath, "ignored\n")

	selectedLocalPath := filepath.Join(syncDir, testFolder, "docs", "report.txt")
	ignoredLocalPath := filepath.Join(syncDir, testFolder, "docs", "other.txt")

	var selectedContent []byte
	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		90*time.Second,
		"sync_paths exact-file download should eventually materialize only the selected file after delta catches up",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			data, err := os.ReadFile(selectedLocalPath)
			if err != nil {
				return false
			}

			if _, err := os.Stat(ignoredLocalPath); !os.IsNotExist(err) {
				return false
			}

			selectedContent = data
			return string(selectedContent) == "selected\n"
		},
		"--download-only",
	)
	assert.Contains(t, attempt.Stderr, "Mode: download-only")
	assert.Equal(t, "selected\n", string(selectedContent))

	_, statErr := os.Stat(ignoredLocalPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// Validates: R-2.4.4
func TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	testFolder := fmt.Sprintf("e2e-sync-marker-%d", time.Now().UnixNano())
	remoteBlockedPath := "/" + testFolder + "/blocked/secret.txt"

	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, fmt.Sprintf(
		"sync_paths = [%q]\nignore_marker = %q\n",
		"/"+testFolder,
		".odignore",
	))
	opsCfgPath := writeMinimalConfig(t)

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)
	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder+"/blocked")
	putRemoteFile(t, opsCfgPath, nil, remoteBlockedPath, "blocked\n")

	markerPath := filepath.Join(syncDir, testFolder, "blocked", ".odignore")
	require.NoError(t, os.MkdirAll(filepath.Dir(markerPath), 0o700))
	require.NoError(t, os.WriteFile(markerPath, []byte("marker"), 0o600))

	_, stderr := runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	blockedLocalPath := filepath.Join(syncDir, testFolder, "blocked", "secret.txt")
	_, statErr := os.Stat(blockedLocalPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	require.NoError(t, os.Remove(markerPath))

	var blockedContent []byte
	attempt := requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		90*time.Second,
		"ignore marker removal should eventually materialize the newly unblocked remote file",
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			data, err := os.ReadFile(blockedLocalPath)
			if err != nil {
				return false
			}

			blockedContent = data
			return string(blockedContent) == "blocked\n"
		},
		"--download-only",
	)
	assert.Contains(t, attempt.Stderr, "Mode: download-only")
	assert.Equal(t, "blocked\n", string(blockedContent))
}
