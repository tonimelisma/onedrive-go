//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestE2E_FixturePreflight_Fast(t *testing.T) {
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, requireSharedFileLink(t))

	assert.Equal(t, fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assert.Equal(t, fixture.FileItem.Selector, fixture.RawStat.SharedSelector)
	assert.NotEmpty(t, fixture.RecipientDriveID)
}
