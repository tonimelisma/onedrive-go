//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-3.6.6, R-3.6.7, R-1.6.2, R-1.2.5
func TestE2E_Shared_FileDiscoveryAndSelectorReadCommands(t *testing.T) {
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

	rawContent := getSharedTargetContent(t, cfgPath, env, "--account", recipientEmail, rawLink)
	selectorContent := getSharedTargetContent(t, cfgPath, env, fileItem.Selector)
	assert.Equal(t, rawContent, selectorContent)
}

// Validates: R-3.6.6, R-3.3.12
func TestE2E_Shared_FileDiscoveryRejectsDriveAdd(t *testing.T) {
	requireDrive2Shared(t)
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	recipientEmail := recipientEmailFromDriveID(t, drive2)
	cfgPath, env := writeSyncConfigForDrive2(t, t.TempDir())

	rawStat := statSharedTargetJSON(t, cfgPath, env, "--account", recipientEmail, rawLink)
	listing := sharedListForRecipient(t, cfgPath, env, recipientEmail)
	fileItem := findSharedItemByRemoteIDs(t, listing.Items, rawStat.RemoteDriveID, rawStat.RemoteItemID, "file")

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "add", fileItem.Selector)
	require.Error(t, err)
	assert.Contains(t, stdout+stderr, "shared files are direct stat/get/put targets")
}
