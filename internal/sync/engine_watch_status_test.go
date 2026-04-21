package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const syncedWatchTestContent = "hello"

func TestRunWatch_BootstrapNoChanges_WritesSyncStatusForBidirectional(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	clock := newManualClock(time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)
	recorder := attachDebugEventRecorder(eng)

	watchCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(watchCtx, SyncBidirectional, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started after bootstrap")

	cancel()
	require.NoError(t, <-done)

	status, err := eng.baseline.ReadSyncStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, clock.Now().UnixNano(), status.LastSyncedAt)
	assert.Zero(t, status.LastFailedCount)
}

func TestProcessDirtyBatch_BidirectionalWritesSyncStatus(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	clock := newManualClock(time.Date(2026, 4, 21, 16, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)
	ctx := t.Context()

	contentHash := hashContentQuickXor(t, syncedWatchTestContent)
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-as",
		ItemType:        ItemTypeFile,
		LocalHash:       contentHash,
		RemoteHash:      contentHash,
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}, "")
	writeLocalFile(t, syncRoot, "already-synced.txt", syncedWatchTestContent)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-as",
		Path:     "already-synced.txt",
		ItemType: ItemTypeFile,
		Hash:     contentHash,
		Size:     5,
	}}, ""))

	dispatch := testWatchRuntime(t, eng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"already-synced.txt"},
	}, bl, SyncBidirectional, safety)
	assert.Nil(t, dispatch)

	status, err := eng.baseline.ReadSyncStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, clock.Now().UnixNano(), status.LastSyncedAt)
}

func TestProcessDirtyBatch_DirectionalDoesNotWriteSyncStatus(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	clock := newManualClock(time.Date(2026, 4, 21, 17, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)
	ctx := t.Context()

	contentHash := hashContentQuickXor(t, syncedWatchTestContent)
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-upload-only",
		ItemType:        ItemTypeFile,
		LocalHash:       contentHash,
		RemoteHash:      contentHash,
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}, "")
	writeLocalFile(t, syncRoot, "already-synced.txt", syncedWatchTestContent)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-upload-only",
		Path:     "already-synced.txt",
		ItemType: ItemTypeFile,
		Hash:     contentHash,
		Size:     5,
	}}, ""))

	dispatch := testWatchRuntime(t, eng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"already-synced.txt"},
	}, bl, SyncUploadOnly, safety)
	assert.Nil(t, dispatch)

	status, err := eng.baseline.ReadSyncStatus(ctx)
	require.NoError(t, err)
	assert.Zero(t, status.LastSyncedAt, "directional watch batches must not persist sync status")
}
