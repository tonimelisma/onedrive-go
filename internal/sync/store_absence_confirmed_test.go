package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.15.3
func TestUndecidedRefreshDelay_DoublesToTheDailyCadence(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 15*time.Minute, undecidedRefreshDelay(1))
	assert.Equal(t, 30*time.Minute, undecidedRefreshDelay(2))
	assert.Equal(t, 2*time.Hour, undecidedRefreshDelay(4))
	assert.Equal(t, FullRemoteRefreshInterval, undecidedRefreshDelay(8))
	assert.Equal(t, FullRemoteRefreshInterval, undecidedRefreshDelay(1000))
}
