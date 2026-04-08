//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-3.6.6, R-3.6.7, R-1.6.2, R-1.2.5, R-1.3.5
func TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip(t *testing.T) {
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	assert.Equal(t, fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assert.Equal(t, fixture.FileItem.Selector, fixture.RawStat.SharedSelector)

	selectorStat := statSharedTargetJSON(t, cfgPath, env, fixture.FileItem.Selector)
	assert.Equal(t, fixture.RawStat.RemoteDriveID, selectorStat.RemoteDriveID)
	assert.Equal(t, fixture.RawStat.RemoteItemID, selectorStat.RemoteItemID)
	assert.Equal(t, fixture.RawStat.Name, selectorStat.Name)
	assert.Equal(t, fixture.FileItem.Selector, selectorStat.SharedSelector)

	originalContent := getSharedTargetContent(t, cfgPath, env, fixture.FileItem.Selector)
	restoreFile := writeTempContentFile(t, originalContent)
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, cfgPath, env, "", "put", restoreFile, fixture.FileItem.Selector)
	})

	updatedContent := fmt.Sprintf("shared-selector-update-%d\n", time.Now().UnixNano())
	updateFile := writeTempContentFile(t, updatedContent)
	runCLIWithoutDrive(t, cfgPath, env, "put", updateFile, fixture.FileItem.Selector)
	eventuallySharedContentEquals(
		t,
		cfgPath,
		env,
		updatedContent,
		"--account",
		fixture.RecipientEmail,
		rawLink,
	)

	runCLIWithoutDrive(t, cfgPath, env, "--account", fixture.RecipientEmail, "put", restoreFile, rawLink)
	eventuallySharedContentEquals(t, cfgPath, env, originalContent, fixture.FileItem.Selector)
}

// Validates: R-3.6.6, R-3.3.5
func TestE2E_Shared_FolderDiscoveryContinuesToDriveAdd(t *testing.T) {
	registerLogDump(t)

	folderFixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, folderFixture.RecipientDriveID, t.TempDir())

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", folderFixture.FolderItem.Selector)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var found bool
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == folderFixture.FolderItem.Selector {
			found = true
			break
		}
	}
	assert.True(t, found, "drive add should configure the writable shared-folder fixture by exact selector identity")
}

// Validates: R-3.3.13
func TestE2E_Shared_RawFolderLinkDriveAdd_NormalizesToCanonicalSharedDrive(t *testing.T) {
	registerLogDump(t)

	rawLink := liveConfig.Fixtures.SharedFolderLink
	if rawLink == "" {
		t.Skip("shared-folder raw-link fixture missing: set ONEDRIVE_TEST_SHARED_FOLDER_LINK in exported env, root .env, or .testdata/fixtures.env")
	}

	folderFixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, folderFixture.RecipientDriveID, t.TempDir())

	rawStat := statSharedTargetJSON(t, cfgPath, env, "--account", folderFixture.RecipientEmail, rawLink)
	assert.Equal(t, folderFixture.FolderItem.RemoteDriveID, rawStat.RemoteDriveID)
	assert.Equal(t, folderFixture.FolderItem.RemoteItemID, rawStat.RemoteItemID)
	assert.Equal(t, folderFixture.FolderItem.Selector, rawStat.SharedSelector)

	runCLIWithoutDrive(t, cfgPath, env, "--account", folderFixture.RecipientEmail, "drive", "add", rawLink)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var found bool
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == folderFixture.FolderItem.Selector {
			found = true
			break
		}
	}
	assert.True(t, found, "drive add <raw-share-url> should normalize to the canonical writable shared-folder selector")
}

// Validates: R-3.6.6, R-3.3.5, R-2.14.2, R-2.14.3, R-2.14.4
func TestE2E_Shared_ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX(t *testing.T) {
	registerLogDump(t)

	readOnlyFixture := resolveSharedFolderFixture(t, liveConfig.Fixtures.ReadOnlySharedFolderSelector)
	cfgPath, env := writeSyncConfigForDriveID(t, readOnlyFixture.RecipientDriveID, t.TempDir())

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", readOnlyFixture.FolderItem.Selector)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var syncDir string
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == readOnlyFixture.FolderItem.Selector {
			syncDir = expandHomePath(parsed.Configured[i].SyncDir, env)
			break
		}
	}
	require.NotEmpty(t, syncDir, "drive list should expose the configured sync_dir for the added read-only shared folder")

	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFixture.FolderItem.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	blockedLocalPath := filepath.Join(syncDir, "blocked-from-recipient.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(blockedLocalPath), 0o700))
	require.NoError(t, os.WriteFile(blockedLocalPath, []byte("recipient write attempt\n"), 0o600))

	_, stderr = runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFixture.FolderItem.Selector, "sync", "--force", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	issuesOut, _ := runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFixture.FolderItem.Selector, "issues")
	assert.Contains(t, issuesOut, "SHARED FOLDER WRITES BLOCKED")
	assert.Contains(t, issuesOut, "Downloads continue normally.")
	assert.Contains(t, issuesOut, "blocked-from-recipient.txt")

	_, stderr = runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFixture.FolderItem.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")
}
