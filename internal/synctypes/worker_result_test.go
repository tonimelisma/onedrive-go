package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.16, R-6.8.4
func TestWorkerResult_ThrottleTargetKey(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("0000000000000001")
	targetDriveID := driveid.New("0000000000000002")

	assert.Empty(t, (*WorkerResult)(nil).ThrottleTargetKey())

	assert.Equal(t, throttleDriveParam(targetDriveID), (&WorkerResult{
		DriveID:       driveID,
		TargetDriveID: targetDriveID,
	}).ThrottleTargetKey())

	assert.Equal(t, throttleDriveParam(driveID), (&WorkerResult{
		DriveID: driveID,
	}).ThrottleTargetKey())

	assert.Equal(t, throttleSharedPrefix+"remote-drive:remote-item", (&WorkerResult{
		DriveID:       driveID,
		TargetDriveID: targetDriveID,
		ShortcutKey:   "remote-drive:remote-item",
	}).ThrottleTargetKey())

	assert.Empty(t, (&WorkerResult{}).ThrottleTargetKey())
}
