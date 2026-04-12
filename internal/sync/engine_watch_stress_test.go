//go:build stress

package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const watchOrderingStressIterations = 8

// Validates: R-2.3.6, R-2.9.3, R-6.10.10
func TestWatchOrderingStress_UserIntentWakeAndWorkerResultRace(t *testing.T) {
	for iteration := 0; iteration < watchOrderingStressIterations; iteration++ {
		eng, _ := newTestEngine(t, nil)
		newTestWatchState(t, eng)
		ctx := t.Context()

		bl, safety := seedApprovedHeldDelete(t, eng, ctx, "stress-delete-result.txt", "stress-item-result")
		busy := testWatchRuntime(t, eng).depGraph.Add(&Action{
			Type:    ActionUpload,
			Path:    "stress-busy-result.txt",
			DriveID: driveid.New(engineTestDriveID),
		}, int64(iteration+1), nil)
		require.NotNil(t, busy)

		results := make(chan WorkerResult, 1)
		results <- WorkerResult{
			ActionID:   busy.ID,
			ActionType: busy.Action.Type,
			Path:       busy.Action.Path,
			DriveID:    busy.Action.DriveID,
			Success:    true,
		}
		userIntentC := make(chan struct{}, 1)
		userIntentC <- struct{}{}
		p := &watchPipeline{
			bl:          bl,
			safety:      safety,
			mode:        SyncBidirectional,
			results:     results,
			userIntentC: userIntentC,
		}

		for step := 0; step < 2; step++ {
			shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
			require.NoError(t, err)
			assert.False(t, shuttingDown)
		}

		assert.False(t, testWatchRuntime(t, eng).userIntentPending)

		outbox := testWatchRuntime(t, eng).currentOutbox()
		require.Len(t, outbox, 1, "either ready-channel order must end with one durable-intent dispatch")
		assertQueuedHeldDeleteAction(t, outbox[0], "stress-delete-result.txt")
		assert.Equal(t, 1, testWatchRuntime(t, eng).depGraph.InFlightCount(), "the busy action should complete before the newly admitted durable-intent action becomes the only in-flight work")
	}
}

// Validates: R-2.3.6, R-2.9.3, R-6.10.10
func TestWatchOrderingStress_RecheckAndUserIntentWakeCoalesce(t *testing.T) {
	for iteration := 0; iteration < watchOrderingStressIterations; iteration++ {
		eng, _ := newTestEngine(t, nil)
		newTestWatchState(t, eng)
		ctx := t.Context()

		bl, safety := seedApprovedHeldDelete(t, eng, ctx, "stress-delete-dual-wake.txt", "stress-item-dual-wake")
		busy := dispatchBusyWatchAction(t, eng, ctx, "stress-busy-dual-wake.txt", int64(iteration+100))

		recheckC := make(chan time.Time, 1)
		recheckC <- time.Now()
		userIntentC := make(chan struct{}, 1)
		userIntentC <- struct{}{}
		p := &watchPipeline{
			bl:          bl,
			safety:      safety,
			mode:        SyncBidirectional,
			recheckC:    recheckC,
			userIntentC: userIntentC,
		}

		for step := 0; step < 2; step++ {
			shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
			require.NoError(t, err)
			assert.False(t, shuttingDown)
			assert.True(t, testWatchRuntime(t, eng).userIntentPending)
			assert.Empty(t, testWatchRuntime(t, eng).currentOutbox())
		}

		ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
		require.True(t, ok)
		assert.Empty(t, ready)
		testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)

		outbox := testWatchRuntime(t, eng).currentOutbox()
		require.Len(t, outbox, 1, "dual wake sources must coalesce into one durable-intent dispatch after capacity returns")
		assertQueuedHeldDeleteAction(t, outbox[0], "stress-delete-dual-wake.txt")
		assert.False(t, testWatchRuntime(t, eng).userIntentPending)
	}
}
