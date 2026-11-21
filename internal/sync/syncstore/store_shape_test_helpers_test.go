package syncstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// seedBlockScopeForTest writes key's block_scopes row, which work blocked
// under key references (the database refuses the blocked row without it).
func seedBlockScopeForTest(t *testing.T, store *SyncStore, key syncdomain.ScopeKey) {
	t.Helper()

	require.NoError(t, store.UpsertBlockScope(t.Context(),
		syncfixture.TestBlockScope(key, time.Minute, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))))
}

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
