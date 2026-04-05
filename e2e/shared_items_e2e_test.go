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

	fixture := resolveSharedFileFixture(t, rawLink)

	assert.Equal(t, fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assert.Equal(t, fixture.FileItem.Selector, fixture.RawStat.SharedSelector)

	selectorStat := statSharedTargetJSON(t, fixture.ConfigPath, fixture.Env, fixture.FileItem.Selector)
	assert.Equal(t, fixture.RawStat.RemoteDriveID, selectorStat.RemoteDriveID)
	assert.Equal(t, fixture.RawStat.RemoteItemID, selectorStat.RemoteItemID)
	assert.Equal(t, fixture.RawStat.Name, selectorStat.Name)
	assert.Equal(t, fixture.FileItem.Selector, selectorStat.SharedSelector)

	originalContent := getSharedTargetContent(t, fixture.ConfigPath, fixture.Env, fixture.FileItem.Selector)
	restoreFile := writeTempContentFile(t, originalContent)
	t.Cleanup(func() {
		_, _, _ = runCLICore(t, fixture.ConfigPath, fixture.Env, "", "put", restoreFile, fixture.FileItem.Selector)
	})

	updatedContent := fmt.Sprintf("shared-selector-update-%d\n", time.Now().UnixNano())
	updateFile := writeTempContentFile(t, updatedContent)
	runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "put", updateFile, fixture.FileItem.Selector)
	eventuallySharedContentEquals(
		t,
		fixture.ConfigPath,
		fixture.Env,
		updatedContent,
		"--account",
		fixture.RecipientEmail,
		rawLink,
	)

	runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "--account", fixture.RecipientEmail, "put", restoreFile, rawLink)
	eventuallySharedContentEquals(t, fixture.ConfigPath, fixture.Env, originalContent, fixture.FileItem.Selector)
}

// Validates: R-3.6.6, R-3.3.5
func TestE2E_Shared_FolderDiscoveryContinuesToDriveAdd(t *testing.T) {
	requireDrive2Shared(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, requireSharedFileLink(t))

	listing := sharedListForRecipient(t, fixture.ConfigPath, fixture.Env, fixture.RecipientEmail)
	var folderItem sharedItemE2E
	for i := range listing.Items {
		if listing.Items[i].Type == "folder" {
			folderItem = listing.Items[i]
			break
		}
	}
	require.NotEmpty(t, folderItem.Selector, "shared listing should include at least one shared folder")

	runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "drive", "add", folderItem.Selector)

	stdout, _ := runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "drive", "list", "--json")
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

	fixture := resolveSharedFileFixture(t, requireSharedFileLink(t))

	listing := sharedListForRecipient(t, fixture.ConfigPath, fixture.Env, fixture.RecipientEmail)
	readOnlyFolder := findSharedItemByNameAndType(t, listing.Items, "Read-only Shared Folder", "folder")

	runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "drive", "add", readOnlyFolder.Selector)

	stdout, _ := runCLIWithoutDrive(t, fixture.ConfigPath, fixture.Env, "drive", "list", "--json")
	var parsed driveListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	var syncDir string
	for i := range parsed.Configured {
		if parsed.Configured[i].CanonicalID == readOnlyFolder.Selector {
			syncDir = expandHomePath(parsed.Configured[i].SyncDir, fixture.Env)
			break
		}
	}
	require.NotEmpty(t, syncDir, "drive list should expose the configured sync_dir for the added read-only shared folder")

	_, stderr := runCLIWithConfigForDrive(t, fixture.ConfigPath, fixture.Env, readOnlyFolder.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")

	blockedLocalPath := filepath.Join(syncDir, "blocked-from-recipient.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(blockedLocalPath), 0o700))
	require.NoError(t, os.WriteFile(blockedLocalPath, []byte("recipient write attempt\n"), 0o600))

	_, stderr = runCLIWithConfigForDrive(t, fixture.ConfigPath, fixture.Env, readOnlyFolder.Selector, "sync", "--force", "--upload-only")
	assert.Contains(t, stderr, "Mode: upload-only")

	issuesOut, _ := runCLIWithConfigForDrive(t, fixture.ConfigPath, fixture.Env, readOnlyFolder.Selector, "issues")
	assert.Contains(t, issuesOut, "SHARED FOLDER WRITES BLOCKED")
	assert.Contains(t, issuesOut, "Downloads continue normally.")
	assert.Contains(t, issuesOut, "blocked-from-recipient.txt")

	_, stderr = runCLIWithConfigForDrive(t, fixture.ConfigPath, fixture.Env, readOnlyFolder.Selector, "sync", "--force", "--download-only")
	assert.Contains(t, stderr, "Mode: download-only")
}
