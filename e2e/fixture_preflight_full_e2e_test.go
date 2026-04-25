//go:build e2e && e2e_full

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestE2E_FixturePreflight_Full(t *testing.T) {
	registerLogDump(t)

	fixtures := []resolvedSharedFolderFixture{
		resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector),
		resolveSharedFolderFixture(t, liveConfig.Fixtures.ReadOnlySharedFolderSelector),
	}

	for _, fixture := range fixtures {
		assert.Equal(t, fixture.RecipientEmail, fixture.FolderItem.AccountEmail)
		assert.Equal(t, "folder", fixture.FolderItem.Type)
		assert.NotEmpty(t, fixture.RecipientDriveID)
	}
	if liveConfig.Fixtures.StandaloneSharedFolderName != "" && fixtures[0].FolderItem.Name != "" {
		assert.Equal(t, liveConfig.Fixtures.StandaloneSharedFolderName, fixtures[0].FolderItem.Name)
	}

	writableShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	readOnlyShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureReadOnly)
	assert.Equal(t, liveConfig.Fixtures.WritableShortcutName, writableShortcut.ShortcutName)
	assert.Equal(t, liveConfig.Fixtures.ReadOnlyShortcutName, readOnlyShortcut.ShortcutName)

	shortcutFixtures := []resolvedShortcutFixture{writableShortcut, readOnlyShortcut}
	for _, fixture := range shortcutFixtures {
		cfgPath, env := writeSyncConfigForDriveID(t, fixture.ParentDrive, t.TempDir())
		requireSharedListContainsShortcutFixture(t, cfgPath, env, fixture)
		requireRootPlaceholderContainsShortcutFixture(t, cfgPath, env, fixture)
	}

	rawLink := liveConfig.Fixtures.SharedFolderLink
	if rawLink == "" {
		return
	}

	writableFixture := fixtures[0]
	cfgPath, env := writeSyncConfigForDriveID(t, writableFixture.RecipientDriveID, t.TempDir())
	rawStat := statSharedTargetJSON(t, cfgPath, env, "--account", writableFixture.RecipientEmail, rawLink)

	assert.Equal(t, writableFixture.FolderItem.RemoteDriveID, rawStat.RemoteDriveID)
	assert.Equal(t, writableFixture.FolderItem.RemoteItemID, rawStat.RemoteItemID)
	assert.Equal(t, writableFixture.FolderItem.Selector, rawStat.SharedSelector)
}
