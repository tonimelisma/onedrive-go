package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3, R-2.1.4
func TestDeferredCountsAddActionAndTotal(t *testing.T) {
	t.Parallel()

	var counts DeferredCounts

	counts.AddAction(nil)
	counts.AddAction(&Action{Type: ActionCleanup})
	counts.AddAction(&Action{Type: ActionUpdateSynced})
	counts.AddAction(&Action{Type: ActionFolderCreate})
	counts.AddAction(&Action{Type: ActionLocalMove})
	counts.AddAction(&Action{Type: ActionRemoteMove})
	counts.AddAction(&Action{Type: ActionDownload})
	counts.AddAction(&Action{Type: ActionUpload})
	counts.AddAction(&Action{Type: ActionLocalDelete})
	counts.AddAction(&Action{Type: ActionRemoteDelete})

	assert.Equal(t, DeferredCounts{
		FolderCreates: 1,
		Moves:         2,
		Downloads:     1,
		Uploads:       1,
		LocalDeletes:  1,
		RemoteDeletes: 1,
	}, counts)
	assert.Equal(t, 7, counts.Total())
}

// Validates: R-2.10.11
func TestActionThrottleTargetKeyUsesNarrowestDriveBoundary(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("drive-remote")

	assert.Empty(t, (*Action)(nil).ThrottleTargetKey())
	assert.Empty(t, (&Action{}).ThrottleTargetKey())
	assert.Equal(t, throttleDriveParam(remoteDriveID), (&Action{DriveID: remoteDriveID}).ThrottleTargetKey())
}
