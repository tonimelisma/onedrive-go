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

	fixture := resolveSharedFileFixture(t, rawLink)

	assert.Equal(t, fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assert.Equal(t, fixture.FileItem.Selector, fixture.RawStat.SharedSelector)

	selectorStat := statSharedTargetJSON(t, fixture.ConfigPath, fixture.Env, fixture.FileItem.Selector)
	assert.Equal(t, fixture.RawStat.RemoteDriveID, selectorStat.RemoteDriveID)
	assert.Equal(t, fixture.RawStat.RemoteItemID, selectorStat.RemoteItemID)
	assert.Equal(t, fixture.RawStat.Name, selectorStat.Name)
	assert.Equal(t, fixture.FileItem.Selector, selectorStat.SharedSelector)

	rawContent := getSharedTargetContent(t, fixture.ConfigPath, fixture.Env, "--account", fixture.RecipientEmail, rawLink)
	selectorContent := getSharedTargetContent(t, fixture.ConfigPath, fixture.Env, fixture.FileItem.Selector)
	assert.Equal(t, rawContent, selectorContent)
}

// Validates: R-3.6.6, R-3.3.12
func TestE2E_Shared_FileDiscoveryRejectsDriveAdd(t *testing.T) {
	requireDrive2Shared(t)
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)

	stdout, stderr, err := runCLICore(t, fixture.ConfigPath, fixture.Env, "", "drive", "add", fixture.FileItem.Selector)
	require.Error(t, err)
	assert.Contains(t, stdout+stderr, "shared files are direct stat/get/put targets")
}
