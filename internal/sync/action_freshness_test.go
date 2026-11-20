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

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.perfCollector = collector
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
	require.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartLocalTruthCount)
}

// Validates: R-2.8.9, R-6.6.17
func TestWorkerStartFreshness_RemoteDownloadMismatchRecordsRemoteTruthCounter(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "remote.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(nil, nil, store, testLogger(t), 1)
	pool.perfCollector = collector
	pool.executeAction(ctx, &TrackedAction{
		ID: 1,
		Action: Action{
			Type:    ActionDownload,
			Path:    "remote.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &PathView{
				Path: "remote.txt",
				Remote: &RemoteState{
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
	require.ErrorIs(t, completion.Err, ErrActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartRemoteTruthCount)
}

// Validates: R-2.8.10, R-6.6.17
func TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability(t *testing.T) {
	t.Parallel()

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(nil, nil, nil, testLogger(t), 2)
	pool.perfCollector = collector

	pool.sendResult(t.Context(), &TrackedAction{
		ID:     1,
		Action: Action{Type: ActionUpload, Path: "local.txt"},
	}, &ActionOutcome{
		Action:            ActionUpload,
		Path:              "local.txt",
		Error:             stalePreconditionError("upload source changed"),
		FailureCapability: PermissionCapabilityLocalRead,
	}, ErrActionPreconditionChanged)
	pool.sendResult(t.Context(), &TrackedAction{
		ID:     2,
		Action: Action{Type: ActionRemoteDelete, Path: "remote.txt"},
	}, &ActionOutcome{
		Action:            ActionRemoteDelete,
		Path:              "remote.txt",
		Error:             stalePreconditionError("remote source changed"),
		FailureCapability: PermissionCapabilityRemoteRead,
	}, ErrActionPreconditionChanged)

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
	pool := NewWorkerPool(nil, nil, nil, testLogger(t), 2)
	pool.perfCollector = collector

	pool.sendResult(t.Context(), &TrackedAction{
		ID:     1,
		Action: Action{Type: ActionUpload, Path: "local.txt"},
	}, &ActionOutcome{
		Action: ActionUpload,
		Path:   "local.txt",
		Error:  stalePreconditionError("upload source changed"),
	}, ErrActionPreconditionChanged)
	pool.sendResult(t.Context(), &TrackedAction{
		ID:     2,
		Action: Action{Type: ActionDownload, Path: "remote.txt"},
	}, &ActionOutcome{
		Action: ActionDownload,
		Path:   "remote.txt",
		Error:  stalePreconditionError("download target changed"),
	}, ErrActionPreconditionChanged)

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
//
// The source of a remote-origin local move must still be the synced item.
// Its content may have changed (D-053: the rename carries the edit, which is
// uploaded next), but a different file at the source path (another identity)
// is not the item OneDrive moved.
func TestWorkerStartFreshness_LocalMoveSourceReplacedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(testDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:             "old.txt",
		ItemType:         ItemTypeFile,
		Hash:             "changed",
		Size:             7,
		LocalDevice:      1,
		LocalInode:       99,
		LocalHasIdentity: true,
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
					Path:             "old.txt",
					DriveID:          remoteDriveID,
					ItemID:           "remote-1",
					ItemType:         ItemTypeFile,
					LocalHash:        "planned",
					LocalSize:        7,
					LocalSizeKnown:   true,
					LocalDevice:      1,
					LocalInode:       2,
					LocalHasIdentity: true,
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

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &Action{
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
func TestActionFreshness_CanceledContextFailsClosedWithoutStoreRead(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.Close(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
		View: &PathView{
			Path:  "upload.txt",
			Local: &LocalState{ItemType: ItemTypeFile, Hash: "planned", Size: 7},
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
	rt.resources.dirtyBuf = NewDirtyBuffer(eng.logger)
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
	flow.sched.graph = NewDepGraph(eng.logger)

	root := flow.sched.graph.Add(&Action{
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

	child := flow.sched.graph.Add(&Action{
		Type: ActionUpload,
		Path: "dependent.txt",
		View: &PathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, child)

	outbox, err := flow.admitReady(ctx, []*TrackedAction{root})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Equal(t, 0, flow.sched.graph.InFlightCount())
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

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &Action{
		Type:    ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &PathView{
			Path: "new.txt",
			Remote: &RemoteState{
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

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "changed-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &Action{
		Type:    ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &PathView{
			Path: "new.txt",
			Remote: &RemoteState{
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

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &Action{
		Type: ActionUpload,
		Path: "upload.txt",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing planner view")
	assert.False(t, decision.Fresh)
}

// Validates: R-2.8.9, R-2.17.3
func TestActionFreshness_LocalOriginMoveRejectsChangedDestination(t *testing.T) {
	t.Parallel()

	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "secret.txt",
		OldPath: "vim.swp",
		ItemID:  "item-swap",
		View:    &PathView{Path: "vim.swp"},
		MovedLocal: &LocalState{
			ItemType: ItemTypeFile, Size: 5, Mtime: 100,
			LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
		},
	}
	planned := &LocalStateRow{
		Path: "secret.txt", ItemType: ItemTypeFile, Size: 5, Mtime: 100,
		LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
	}

	fresh := evaluateMoveEndpointFreshness(action, actionFreshnessInputs{
		LocalTruthComplete:   true,
		LocalAtMovePeer:      planned,
		LocalAtMovePeerFound: true,
	})
	assert.True(t, fresh.Fresh)

	changed := *planned
	changed.Mtime = 200
	stale := evaluateMoveEndpointFreshness(action, actionFreshnessInputs{
		LocalTruthComplete:   true,
		LocalAtMovePeer:      &changed,
		LocalAtMovePeerFound: true,
	})
	assert.False(t, stale.Fresh, "a destination that changed since planning must not be moved onto")
}

// Validates: R-2.17.1
//
// A move's destination whose birth time changed since planning is another
// entry holding the planned inode number: the move is stale. A side without
// a birth time compares (device, inode) only.
func TestActionFreshness_LocalOriginMoveRejectsADestinationBornAgain(t *testing.T) {
	t.Parallel()

	const born = int64(1_700_000_000_000_000_001)
	action := &Action{
		Type:    ActionRemoteMove,
		Path:    "new",
		OldPath: "old",
		ItemID:  "item-old",
		View:    &PathView{Path: "old"},
		MovedLocal: &LocalState{
			ItemType: ItemTypeFolder, LocalDevice: 7, LocalInode: 8, LocalBirthTime: born, LocalHasIdentity: true,
		},
	}
	for _, tc := range []struct {
		name      string
		birthTime int64
		fresh     bool
	}{
		{name: "same entry", birthTime: born, fresh: true},
		{name: "born again", birthTime: born + 1},
		{name: "no birth time observed", fresh: true},
	} {
		destination := &LocalStateRow{
			Path: "new", ItemType: ItemTypeFolder,
			LocalDevice: 7, LocalInode: 8, LocalBirthTime: tc.birthTime, LocalHasIdentity: true,
		}
		got := evaluateMoveEndpointFreshness(action, actionFreshnessInputs{
			LocalTruthComplete:   true,
			LocalAtMovePeer:      destination,
			LocalAtMovePeerFound: true,
		})
		assert.Equal(t, tc.fresh, got.Fresh, tc.name)
	}
}
