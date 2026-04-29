package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []LocalStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))

	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &TrackedAction{
		ID: 1,
		Action: Action{
			Type: ActionUpload,
			Path: "upload.txt",
			View: &PathView{
				Path:  "upload.txt",
				Local: &LocalState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_DownloadDestinationAppearedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "download.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-now",
		Size:     9,
	}}))

	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &TrackedAction{
		ID: 1,
		Action: Action{
			Type: ActionDownload,
			Path: "download.txt",
			View: &PathView{
				Path: "download.txt",
				Remote: &RemoteState{
					ItemType: ItemTypeFile,
					Hash:     "remote-planned",
					Size:     9,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalDeleteMissingIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, nil))

	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &TrackedAction{
		ID: 1,
		Action: Action{
			Type: ActionLocalDelete,
			Path: "delete.txt",
			View: &PathView{
				Path:  "delete.txt",
				Local: &LocalState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalMoveSourceChangedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "old.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &TrackedAction{
		ID: 1,
		Action: Action{
			Type:    ActionLocalMove,
			Path:    "new.txt",
			OldPath: "old.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &PathView{
				Path: "new.txt",
				Remote: &RemoteState{
					DriveID:  remoteDriveID,
					ItemID:   "remote-1",
					ItemType: ItemTypeFile,
					Hash:     "remote",
					Size:     7,
				},
				Baseline: &BaselineEntry{
					Path:           "old.txt",
					DriveID:        remoteDriveID,
					ItemID:         "remote-1",
					ItemType:       ItemTypeFile,
					LocalHash:      "planned",
					LocalSize:      7,
					LocalSizeKnown: true,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []LocalStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))
	require.NoError(t, store.MarkLocalTruthSuspect(ctx, LocalTruthRecoveryDroppedEvents))

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
		View: &PathView{
			Path:  "upload.txt",
			Local: &LocalState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
		},
	})

	require.NoError(t, err)
	assert.True(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "download.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     12,
		ETag:     "etag-changed",
	}}, "delta-1", remoteDriveID))

	flow.initializeRuntimeState(&runtimePlan{})
	flow.depGraph = NewDepGraph(eng.logger)

	root := flow.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "download.txt",
		ItemID:  "remote-1",
		DriveID: remoteDriveID,
		View: &PathView{
			Path: "download.txt",
			Remote: &RemoteState{
				DriveID:  remoteDriveID,
				ItemID:   "remote-1",
				ItemType: ItemTypeFile,
				Hash:     "planned",
				Size:     12,
				ETag:     "etag-planned",
			},
		},
	}, 1, nil)
	require.NotNil(t, root)

	child := flow.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "dependent.txt",
		View: &PathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := flow.admitReady(ctx, rt, []*TrackedAction{root})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, flow.depGraph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
	require.NotNil(t, rt.dirtyBuf.FlushImmediate())
}
