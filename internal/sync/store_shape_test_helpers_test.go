package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func testBlockedRetryWorkRow(
	path string,
	oldPath string,
	actionType ActionType,
	scopeKey ScopeKey,
	attemptCount int,
) *RetryWorkRow {
	return &RetryWorkRow{
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
) ObservationState {
	return ObservationState{
		ContentDriveID:          contentDriveID,
		Cursor:                  cursor,
		NextFullRemoteRefreshAt: nextFullRemoteRefreshAt,
	}
}
