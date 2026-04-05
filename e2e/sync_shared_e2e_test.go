//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configuredSyncDirForCanonicalID(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
) string {
	t.Helper()

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID != canonicalID {
			continue
		}

		return expandHomePath(parsed.Configured[i].SyncDir, env)
	}

	require.Failf(t, "configured shared drive not found", "canonical_id=%s", canonicalID)
	return ""
}

// Validates: R-3.6.1
func TestE2E_SharedFolder_DriveList_ShowsExplicitSharedFixtures(t *testing.T) {
	registerLogDump(t)

	for _, fixture := range []resolvedSharedFolderFixture{
		resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector),
		resolveSharedFolderFixture(t, liveConfig.Fixtures.ReadOnlySharedFolderSelector),
	} {
		cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

		stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
		var parsed driveListE2EOutput
		require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

		available := map[string]struct{}{}
		for i := range parsed.Available {
			available[parsed.Available[i].CanonicalID] = struct{}{}
		}

		_, found := available[fixture.FolderItem.Selector]
		assert.True(t, found, "drive list should expose the shared-folder fixture by exact selector")
	}
}

func TestE2E_SharedFolder_RemoteMutationSyncsToRecipient(t *testing.T) {
	registerLogDump(t)

	fixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", fixture.FolderItem.Selector)

	syncDir := configuredSyncDirForCanonicalID(t, cfgPath, env, fixture.FolderItem.Selector)
	remoteName := fmt.Sprintf("shared-sync-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("shared sync content %d\n", time.Now().UnixNano())
	contentFile := writeTempContentFile(t, expectedContent)
	localPath := filepath.Join(syncDir, remoteName)

	t.Cleanup(func() {
		_, _, _ = runCLICore(t, cfgPath, env, fixture.FolderItem.Selector, "rm", remotePath)
	})

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.FolderItem.Selector, "put", contentFile, remotePath)

	require.Eventually(t, func() bool {
		_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, fixture.FolderItem.Selector, "sync", "--force", "--download-only")
		if !strings.Contains(stderr, "Mode: download-only") {
			return false
		}

		data, err := os.ReadFile(localPath)
		return err == nil && string(data) == expectedContent
	}, pollTimeout, 2*time.Second)
}

func TestE2E_SharedFolder_RecipientSyncTwice_Idempotent(t *testing.T) {
	registerLogDump(t)

	fixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", fixture.FolderItem.Selector)

	syncDir := configuredSyncDirForCanonicalID(t, cfgPath, env, fixture.FolderItem.Selector)
	remoteName := fmt.Sprintf("shared-idempotent-%d.txt", time.Now().UnixNano())
	remotePath := "/" + remoteName
	expectedContent := fmt.Sprintf("shared sync idempotent %d\n", time.Now().UnixNano())
	contentFile := writeTempContentFile(t, expectedContent)
	localPath := filepath.Join(syncDir, remoteName)

	t.Cleanup(func() {
		_, _, _ = runCLICore(t, cfgPath, env, fixture.FolderItem.Selector, "rm", remotePath)
	})

	runCLIWithConfigForDrive(t, cfgPath, env, fixture.FolderItem.Selector, "put", contentFile, remotePath)

	require.Eventually(t, func() bool {
		_, _ = runCLIWithConfigForDrive(t, cfgPath, env, fixture.FolderItem.Selector, "sync", "--force", "--download-only")
		data, err := os.ReadFile(localPath)
		return err == nil && string(data) == expectedContent
	}, pollTimeout, 2*time.Second)

	stderr := assertSyncLeavesLocalTreeStableForDrive(
		t,
		cfgPath,
		env,
		fixture.FolderItem.Selector,
		syncDir,
		"sync",
		"--force",
		"--download-only",
	)
	assert.Contains(t, stderr, "Mode: download-only")
}
