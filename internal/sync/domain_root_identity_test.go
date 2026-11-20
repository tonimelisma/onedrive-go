package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.2.13
func TestScopePathMatches_RootBoundaryCoversEverything(t *testing.T) {
	t.Parallel()

	assert.True(t, scopePathMatches("Docs/a.txt", "."))
	assert.True(t, scopePathMatches("c.txt", ""))
	assert.False(t, scopePathMatches("Docs2/a.txt", "Docs"))
}
