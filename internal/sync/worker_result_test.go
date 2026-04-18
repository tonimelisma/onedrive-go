package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.16, R-6.8.4
func TestActionCompletion_ThrottleTargetKey(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("0000000000000001")
	targetDriveID := driveid.New("0000000000000002")

	assert.Empty(t, (*ActionCompletion)(nil).ThrottleTargetKey())

	assert.Equal(t, throttleDriveParam(targetDriveID), (&ActionCompletion{
		DriveID:       driveID,
		TargetDriveID: targetDriveID,
	}).ThrottleTargetKey())

	assert.Equal(t, throttleDriveParam(driveID), (&ActionCompletion{
		DriveID: driveID,
	}).ThrottleTargetKey())

	assert.Empty(t, (&ActionCompletion{}).ThrottleTargetKey())
}
