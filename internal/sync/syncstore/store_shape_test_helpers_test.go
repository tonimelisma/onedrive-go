package syncstore

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func testBlockedRetryWorkRow(
	path string,
	oldPath string,
	actionType syncdomain.ActionType,
	scopeKey syncdomain.ScopeKey,
	attemptCount int,
) *syncdomain.RetryWorkRow {
	return &syncdomain.RetryWorkRow{
		Path:         path,
		OldPath:      oldPath,
		ActionType:   actionType,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: attemptCount,
	}
}

func testObservationState(
	contentDriveID driveid.ID,
	cursor string,
	nextFullRemoteRefreshAt int64,
) syncdomain.ObservationState {
	return syncdomain.ObservationState{
		ContentDriveID:          contentDriveID,
		Cursor:                  cursor,
		NextFullRemoteRefreshAt: nextFullRemoteRefreshAt,
	}
}
