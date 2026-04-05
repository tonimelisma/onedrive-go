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
	requireDrive2Shared(t)
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	recipientEmail := recipientEmailFromDriveID(t, drive2)
	cfgPath, env := writeSyncConfigForDrive2(t, t.TempDir())

	rawStat := statSharedTargetJSON(t, cfgPath, env, "--account", recipientEmail, rawLink)
	listing := sharedListForRecipient(t, cfgPath, env, recipientEmail)
	fileItem := findSharedItemByRemoteIDs(t, listing.Items, rawStat.RemoteDriveID, rawStat.RemoteItemID, "file")

	assert.Equal(t, recipientEmail, fileItem.AccountEmail)
	assert.Equal(t, fileItem.Selector, rawStat.SharedSelector)

	selectorStat := statSharedTargetJSON(t, cfgPath, env, fileItem.Selector)
	assert.Equal(t, rawStat.RemoteDriveID, selectorStat.RemoteDriveID)
	assert.Equal(t, rawStat.RemoteItemID, selectorStat.RemoteItemID)
	assert.Equal(t, rawStat.Name, selectorStat.Name)
	assert.Equal(t, fileItem.Selector, selectorStat.SharedSelector)

	originalContent := getSharedTargetContent(t, cfgPath, env, fileItem.Selector)
	restoreFile := writeTempContentFile(t, originalContent)
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, cfgPath, env, "", "put", restoreFile, fileItem.Selector)
	})

	updatedContent := fmt.Sprintf("shared-selector-update-%d\n", time.Now().UnixNano())
	updateFile := writeTempContentFile(t, updatedContent)
	runCLIWithoutDrive(t, cfgPath, env, "put", updateFile, fileItem.Selector)
	eventuallySharedContentEquals(t, cfgPath, env, updatedContent, "--account", recipientEmail, rawLink)

	runCLIWithoutDrive(t, cfgPath, env, "--account", recipientEmail, "put", restoreFile, rawLink)
	eventuallySharedContentEquals(t, cfgPath, env, originalContent, fileItem.Selector)
}

// Validates: R-3.6.6, R-3.3.5
func TestE2E_Shared_FolderDiscoveryContinuesToDriveAdd(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	recipientEmail := recipientEmailFromDriveID(t, drive2)
	cfgPath, env := writeSyncConfigForDrive2(t, t.TempDir())

	listing := sharedListForRecipient(t, cfgPath, env, recipientEmail)
	var folderItem sharedItemE2E
	for i := range listing.Items {
		if listing.Items[i].Type == "folder" {
			folderItem = listing.Items[i]
			break
		}
	}
	require.NotEmpty(t, folderItem.Selector, "shared listing should include at least one shared folder")

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", folderItem.Selector)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var found bool
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == folderItem.Selector {
			found = true
			break
		}
	}
	assert.True(t, found, "drive add should configure the shared folder discovered by 'shared'")
}

// Validates: R-3.6.6, R-3.3.5, R-2.14.2, R-2.14.3, R-2.14.4
func TestE2E_Shared_ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	recipientEmail := recipientEmailFromDriveID(t, drive2)
	cfgPath, env := writeSyncConfigForDrive2(t, t.TempDir())

	listing := sharedListForRecipient(t, cfgPath, env, recipientEmail)
	readOnlyFolder := findSharedItemByNameAndType(t, listing.Items, "Read-only Shared Folder", "folder")

	runCLIWithoutDrive(t, cfgPath, env, "drive", "add", readOnlyFolder.Selector)

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var syncDir string
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == readOnlyFolder.Selector {
			syncDir = expandHomePath(parsed.Configured[i].SyncDir, env)
			break
		}
	}
	require.NotEmpty(t, syncDir, "drive list should expose the configured sync_dir for the added read-only shared folder")

	_, stderr := runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFolder.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	blockedLocalPath := filepath.Join(syncDir, "blocked-from-recipient.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(blockedLocalPath), 0o700))
	require.NoError(t, os.WriteFile(blockedLocalPath, []byte("recipient write attempt\n"), 0o600))

	_, stderr = runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFolder.Selector, "sync", "--force", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	issuesOut, _ := runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFolder.Selector, "issues")
	assert.Contains(t, issuesOut, "SHARED FOLDER WRITES BLOCKED")
	assert.Contains(t, issuesOut, "Downloads continue normally.")
	assert.Contains(t, issuesOut, "blocked-from-recipient.txt")

	_, stderr = runCLIWithConfigForDrive(t, cfgPath, env, readOnlyFolder.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")
}
