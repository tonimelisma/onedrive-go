package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.11
func TestActionThrottleTargetKeyUsesNarrowestDriveBoundary(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("drive-remote")

	assert.Empty(t, (*Action)(nil).ThrottleTargetKey())
	assert.Empty(t, (&Action{}).ThrottleTargetKey())
	assert.Equal(t, throttleDriveParam(remoteDriveID), (&Action{DriveID: remoteDriveID}).ThrottleTargetKey())
}
