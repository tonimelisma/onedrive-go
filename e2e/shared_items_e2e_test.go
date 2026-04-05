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

type sharedItemE2E struct {
	Selector      string `json:"selector"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountEmail  string `json:"account_email"`
	SharedByEmail string `json:"shared_by_email"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

type sharedListE2EOutput struct {
	Items []sharedItemE2E `json:"items"`
}

type sharedStatE2EOutput struct {
	Name           string `json:"name"`
	AccountEmail   string `json:"account_email"`
	RemoteDriveID  string `json:"remote_drive_id"`
	RemoteItemID   string `json:"remote_item_id"`
	SharedSelector string `json:"shared_selector"`
}

type driveListE2EOutput struct {
	Configured []struct {
		CanonicalID string `json:"canonical_id"`
		SyncDir     string `json:"sync_dir"`
	} `json:"configured"`
}

func requireSharedFileLink(t *testing.T) string {
	t.Helper()

	rawLink := os.Getenv("ONEDRIVE_TEST_SHARED_LINK")
	if rawLink == "" {
		t.Skip("set ONEDRIVE_TEST_SHARED_LINK in .env to run shared-file E2E")
	}

	return rawLink
}

func expandHomePath(path string, env map[string]string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home := env["HOME"]
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return path
	}

	return filepath.Join(home, path[2:])
}

func runCLIWithoutDrive(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s", args, stdout, stderr)

	return stdout, stderr
}

func sharedListForRecipient(t *testing.T, cfgPath string, env map[string]string, recipientEmail string) sharedListE2EOutput {
	t.Helper()

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "--account", recipientEmail, "shared", "--json")

	var parsed sharedListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
}

func statSharedTargetJSON(t *testing.T, cfgPath string, env map[string]string, args ...string) sharedStatE2EOutput {
	t.Helper()

	fullArgs := append([]string{"stat", "--json"}, args...)
	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	var parsed sharedStatE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
}

func getSharedTargetContent(t *testing.T, cfgPath string, env map[string]string, args ...string) string {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "downloaded")
	fullArgs := append([]string{"get"}, args...)
	fullArgs = append(fullArgs, localPath)
	runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	data, err := os.ReadFile(localPath)
	require.NoError(t, err)

	return string(data)
}

func writeTempContentFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "upload.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func eventuallySharedContentEquals(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	expected string,
	args ...string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		localPath := filepath.Join(t.TempDir(), "downloaded")
		fullArgs := append([]string{"get"}, args...)
		fullArgs = append(fullArgs, localPath)

		_, _, err := runCLICore(t, cfgPath, env, "", fullArgs...)
		if err != nil {
			return false
		}

		data, readErr := os.ReadFile(localPath)
		if readErr != nil {
			return false
		}

		return string(data) == expected
	}, pollTimeout, 2*time.Second)
}

func findSharedItemByRemoteIDs(t *testing.T, items []sharedItemE2E, driveID, itemID, itemType string) sharedItemE2E {
	t.Helper()

	for i := range items {
		if items[i].RemoteDriveID == driveID && items[i].RemoteItemID == itemID && items[i].Type == itemType {
			return items[i]
		}
	}

	require.Failf(t, "shared item not found", "drive=%s item=%s type=%s", driveID, itemID, itemType)
	return sharedItemE2E{}
}

func findSharedItemByNameAndType(t *testing.T, items []sharedItemE2E, name, itemType string) sharedItemE2E {
	t.Helper()

	for i := range items {
		if items[i].Name == name && items[i].Type == itemType {
			return items[i]
		}
	}

	require.Failf(t, "shared item not found", "name=%s type=%s", name, itemType)
	return sharedItemE2E{}
}

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
