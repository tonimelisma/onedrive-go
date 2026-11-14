//go:build e2e && e2e_full

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-3.6.6, R-3.6.7, R-1.6.2, R-1.2.5
func TestE2E_Shared_FileDiscoveryAndSelectorReadCommands(t *testing.T) {
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	assertSharedFileFixtureConsistent(t, &fixture)

	selectorStat := statSharedTargetJSON(t, cfgPath, env, fixture.FileItem.Selector)
	assertStatNamesSharedItem(t, &fixture.FileItem, &selectorStat)
	assert.Equal(t, fixture.RawStat.Name, selectorStat.Name)

	rawContent := getSharedTargetContent(t, cfgPath, env, "--account", fixture.RecipientEmail, rawLink)
	selectorContent := getSharedTargetContent(t, cfgPath, env, fixture.FileItem.Selector)
	assert.Equal(t, rawContent, selectorContent)
}

// Validates: R-3.6.6, R-3.3.12
func TestE2E_Shared_FileDiscoveryRejectsDriveAdd(t *testing.T) {
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "add", fixture.FileItem.Selector)
	require.Error(t, err)
	assert.Contains(t, stdout+stderr, "shared files are direct stat/get/put targets")
}
