//go:build e2e && e2e_full

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestE2E_FixturePreflight_Full(t *testing.T) {
	registerLogDump(t)

	for _, fixture := range []resolvedSharedFolderFixture{
		resolveSharedFolderFixture(t, liveConfig.Fixtures.WritableSharedFolderSelector),
		resolveSharedFolderFixture(t, liveConfig.Fixtures.ReadOnlySharedFolderSelector),
	} {
		assert.Equal(t, fixture.RecipientEmail, fixture.FolderItem.AccountEmail)
		assert.Equal(t, "folder", fixture.FolderItem.Type)
		assert.NotEmpty(t, fixture.RecipientDriveID)
	}
}
