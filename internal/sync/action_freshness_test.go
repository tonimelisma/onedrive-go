package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Validates: R-2.8.9, R-6.6.17
func TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []localStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))

	collector := perf.NewCollector(nil)
	pool := newWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.perfCollector = collector
	pool.executeAction(ctx, &trackedAction{
		ID: 1,
		Action: Action{
			Type: ActionUpload,
			Path: "upload.txt",
			View: &pathView{
				Path:  "upload.txt",
				Local: &localState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	require.ErrorIs(t, completion.Err, errActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartLocalTruthCount)
}

// Validates: R-2.8.9, R-6.6.17
func TestWorkerStartFreshness_RemoteDownloadMismatchRecordsRemoteTruthCounter(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "remote.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	collector := perf.NewCollector(nil)
	pool := newWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.perfCollector = collector
	pool.executeAction(ctx, &trackedAction{
		ID: 1,
		Action: Action{
			Type:    ActionDownload,
			Path:    "remote.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &pathView{
				Path: "remote.txt",
				Remote: &remoteState{
					DriveID:  remoteDriveID,
					ItemID:   "remote-1",
					ItemType: ItemTypeFile,
					Hash:     "planned",
					Size:     7,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	require.ErrorIs(t, completion.Err, errActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartRemoteTruthCount)
}

// Validates: R-2.8.10, R-6.6.17
func TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability(t *testing.T) {
	t.Parallel()

	collector := perf.NewCollector(nil)
	pool := newWorkerPool(nil, nil, nil, testLogger(t), 2)
	pool.perfCollector = collector

	pool.sendResult(t.Context(), &trackedAction{
		ID:     1,
		Action: Action{Type: ActionUpload, Path: "local.txt"},
	}, &actionOutcome{
		Action:            ActionUpload,
		Path:              "local.txt",
		Error:             stalePreconditionError("upload source changed"),
		FailureCapability: permissionCapabilityLocalRead,
	}, errActionPreconditionChanged)
	pool.sendResult(t.Context(), &trackedAction{
		ID:     2,
		Action: Action{Type: ActionRemoteDelete, Path: "remote.txt"},
	}, &actionOutcome{
		Action:            ActionRemoteDelete,
		Path:              "remote.txt",
		Error:             stalePreconditionError("remote source changed"),
		FailureCapability: permissionCapabilityRemoteRead,
	}, errActionPreconditionChanged)

	<-pool.Completions()
	<-pool.Completions()

	snapshot := collector.Snapshot()
	assert.Equal(t, 1, snapshot.SupersededLiveLocalPreconditionCount)
	assert.Equal(t, 1, snapshot.SupersededLiveRemotePreconditionCount)
}

// Validates: R-2.8.10, R-6.6.17
func TestWorkerPool_SendResultDoesNotGuessLivePreconditionSourceWithoutCapability(t *testing.T) {
	t.Parallel()

	collector := perf.NewCollector(nil)
	pool := newWorkerPool(nil, nil, nil, testLogger(t), 2)
	pool.perfCollector = collector

	pool.sendResult(t.Context(), &trackedAction{
		ID:     1,
		Action: Action{Type: ActionUpload, Path: "local.txt"},
	}, &actionOutcome{
		Action: ActionUpload,
		Path:   "local.txt",
		Error:  stalePreconditionError("upload source changed"),
	}, errActionPreconditionChanged)
	pool.sendResult(t.Context(), &trackedAction{
		ID:     2,
		Action: Action{Type: ActionDownload, Path: "remote.txt"},
	}, &actionOutcome{
		Action: ActionDownload,
		Path:   "remote.txt",
		Error:  stalePreconditionError("download target changed"),
	}, errActionPreconditionChanged)

	<-pool.Completions()
	<-pool.Completions()

	snapshot := collector.Snapshot()
	assert.Equal(t, 0, snapshot.SupersededLiveLocalPreconditionCount)
	assert.Equal(t, 0, snapshot.SupersededLiveRemotePreconditionCount)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_DownloadDestinationAppearedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "download.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-now",
		Size:     9,
	}}))

	pool := newWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &trackedAction{
		ID: 1,
		Action: Action{
			Type: ActionDownload,
			Path: "download.txt",
			View: &pathView{
				Path: "download.txt",
				Remote: &remoteState{
					ItemType: ItemTypeFile,
					Hash:     "remote-planned",
					Size:     9,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, errActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalDeleteMissingIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, nil))

	pool := newWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &trackedAction{
		ID: 1,
		Action: Action{
			Type: ActionLocalDelete,
			Path: "delete.txt",
			View: &pathView{
				Path:  "delete.txt",
				Local: &localState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, errActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalMoveSourceChangedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "old.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	pool := newWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.executeAction(ctx, &trackedAction{
		ID: 1,
		Action: Action{
			Type:    ActionLocalMove,
			Path:    "new.txt",
			OldPath: "old.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &pathView{
				Path: "new.txt",
				Remote: &remoteState{
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
	assert.ErrorIs(t, completion.Err, errActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []localStateRow{{
		Path:     "upload.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))
	require.NoError(t, store.MarkLocalTruthSuspect(ctx, localTruthRecoveryDroppedEvents))

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
		View: &pathView{
			Path:  "upload.txt",
			Local: &localState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
		},
	})

	require.NoError(t, err)
	assert.True(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestActionFreshness_CanceledContextFailsClosedWithoutStoreRead(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.Close(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
		View: &pathView{
			Path:  "upload.txt",
			Local: &localState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
		},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, decision.Fresh)
}

// Validates: R-2.8.9, R-6.6.17
func TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.perfCollector = perf.NewCollector(nil)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = newDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []observedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "download.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     12,
		ETag:     "etag-changed",
	}}, "delta-1", remoteDriveID))

	flow.initializeRuntimeState(&runtimePlan{})
	flow.depGraph = newDepGraph(eng.logger)

	root := flow.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "download.txt",
		ItemID:  "remote-1",
		DriveID: remoteDriveID,
		View: &pathView{
			Path: "download.txt",
			Remote: &remoteState{
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
		View: &pathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := flow.admitReady(ctx, []*trackedAction{root})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, flow.depGraph.InFlightCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
	require.NotNil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Equal(t, 1, eng.collector().Snapshot().SupersededEngineAdmissionCount)
}

// Validates: R-2.8.9
func TestActionFreshness_PostRemoteMoveUploadAllowsMoveProducedETagChange(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type:    ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &pathView{
			Path: "new.txt",
			Remote: &remoteState{
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: ItemTypeFile,
				Hash:     "old-hash",
				Size:     10,
				Mtime:    1,
				ETag:     "etag-before-move",
			},
			Baseline: &BaselineEntry{
				Path:     "old.txt",
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: ItemTypeFile,
			},
		},
	})

	require.NoError(t, err)
	assert.True(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestActionFreshness_PostRemoteMoveUploadRejectsRemoteContentChange(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type:    ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &pathView{
			Path: "new.txt",
			Remote: &remoteState{
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: ItemTypeFile,
				Hash:     "old-hash",
				Size:     10,
				Mtime:    1,
				ETag:     "etag-before-move",
			},
			Baseline: &BaselineEntry{
				Path:     "old.txt",
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: ItemTypeFile,
			},
		},
	})

	require.NoError(t, err)
	assert.False(t, decision.Fresh)
	assert.Contains(t, decision.Reason, "remote truth changed")
}

// Validates: R-2.8.9
func TestActionFreshness_MissingPlannerViewFailsClosedForExecutableAction(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, nil))

	decision, err := evaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing planner view")
	assert.False(t, decision.Fresh)
}
