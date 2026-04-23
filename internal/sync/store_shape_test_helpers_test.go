package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func testDelayedRetryWorkRow(
	path string,
	oldPath string,
	actionType ActionType,
	attemptCount int,
	nextRetryAt int64,
) *RetryWorkRow {
	return &RetryWorkRow{
		Path:         path,
		OldPath:      oldPath,
		ActionType:   actionType,
		AttemptCount: attemptCount,
		NextRetryAt:  nextRetryAt,
	}
}

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

func testBlockScope(
	scopeKey ScopeKey,
	trialInterval time.Duration,
	nextTrialAt time.Time,
) *BlockScope {
	return &BlockScope{
		Key:           scopeKey,
		TrialInterval: trialInterval,
		NextTrialAt:   nextTrialAt,
	}
}

func testObservationState(
	mountDriveID driveid.ID,
	cursor string,
	nextFullRemoteRefreshAt int64,
) ObservationState {
	return ObservationState{
		MountDriveID:            mountDriveID,
		Cursor:                  cursor,
		NextFullRemoteRefreshAt: nextFullRemoteRefreshAt,
	}
}

func readObservationStateForTest(
	tb testing.TB,
	store *SyncStore,
	ctx context.Context,
) ObservationState {
	tb.Helper()

	state, err := store.ReadObservationState(ctx)
	require.NoError(tb, err)
	require.NotNil(tb, state)
	return *state
}
