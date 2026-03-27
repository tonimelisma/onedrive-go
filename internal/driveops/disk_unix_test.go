//go:build darwin || linux

package driveops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.6
func TestDiskAvailable_ReturnsPositiveValue(t *testing.T) {
	t.Parallel()

	avail, err := DiskAvailable(".")
	require.NoError(t, err)
	assert.Positive(t, avail, "disk available should be > 0 for current directory")
}
