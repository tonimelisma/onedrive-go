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

	assertSharedFileFixtureConsistent(t, &fixture)
	assert.NotEmpty(t, fixture.RecipientDriveID)

	writableShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureWritable)
	readOnlyShortcut := requireShortcutFixtureWithCatalog(t, shortcutFixtureReadOnly)

	assertSameIdentity(t, "the writable shortcut's shared item account",
		writableShortcut.ParentEmail, writableShortcut.SharedItem.AccountEmail)
	assertSameIdentity(t, "the read-only shortcut's shared item account",
		readOnlyShortcut.ParentEmail, readOnlyShortcut.SharedItem.AccountEmail)
	assert.NotEmpty(t, writableShortcut.SharedItem.RemoteDriveID)
	assert.NotEmpty(t, readOnlyShortcut.SharedItem.RemoteDriveID)
}
