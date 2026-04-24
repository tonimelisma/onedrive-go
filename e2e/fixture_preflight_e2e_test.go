//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestE2E_FixturePreflight_Fast(t *testing.T) {
	registerLogDump(t)
	requireVerifierOwnedPreflightEnv(t, e2eRunFastFixturePreflightEnvVar)

	fixture := resolveSharedFileFixture(t, requireSharedFileLink(t))

	assert.Equal(t, fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assert.Equal(t, fixture.FileItem.Selector, fixture.RawStat.SharedSelector)
	assert.NotEmpty(t, fixture.RecipientDriveID)

	writableShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	readOnlyShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureReadOnly)

	assert.Equal(t, writableShortcut.ParentEmail, writableShortcut.SharedItem.AccountEmail)
	assert.Equal(t, readOnlyShortcut.ParentEmail, readOnlyShortcut.SharedItem.AccountEmail)
	assert.NotEmpty(t, writableShortcut.SharedItem.RemoteDriveID)
	assert.NotEmpty(t, readOnlyShortcut.SharedItem.RemoteDriveID)
}
