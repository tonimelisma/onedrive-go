package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.8.4, R-6.8.12
func TestAction_ThrottleTargetKey(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("0000000000000001")
	targetDriveID := driveid.New("0000000000000002")

	tests := []struct {
		name   string
		action *Action
		want   string
	}{
		{
			name:   "nil action",
			action: nil,
			want:   "",
		},
		{
			name: "uses target drive id when present",
			action: &Action{
				DriveID:       driveID,
				TargetDriveID: targetDriveID,
			},
			want: throttleDriveParam(targetDriveID),
		},
		{
			name: "falls back to planned drive id",
			action: &Action{
				DriveID: driveID,
			},
			want: throttleDriveParam(driveID),
		},
		{
			name: "shared target uses shared boundary",
			action: &Action{
				DriveID:           driveID,
				TargetDriveID:     targetDriveID,
				TargetShortcutKey: "remote-drive:remote-item",
			},
			want: throttleSharedPrefix + "remote-drive:remote-item",
		},
		{
			name:   "zero value action",
			action: &Action{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.action.ThrottleTargetKey())
		})
	}
}
