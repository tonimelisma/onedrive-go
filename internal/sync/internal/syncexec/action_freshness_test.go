package syncexec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.8.9, R-6.6.17
func TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "upload.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []syncdomain.LocalStateRow{{
		Path:     "upload.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(WorkerPoolInputs{Store: store, Logger: syncfixture.TestLogger(t), CompletionBuffer: 1, Perf: collector})
	pool.ExecuteActionForTest(ctx, &syncdomain.TrackedAction{
		ID: 1,
		Action: syncdomain.Action{
			Type: syncdomain.ActionUpload,
			Path: "upload.txt",
			View: &syncdomain.PathView{
				Path:  "upload.txt",
				Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	require.ErrorIs(t, completion.Err, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartLocalTruthCount)
}

// Validates: R-2.8.9, R-6.6.17
func TestWorkerStartFreshness_RemoteDownloadMismatchRecordsRemoteTruthCounter(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "remote.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(WorkerPoolInputs{Store: store, Logger: syncfixture.TestLogger(t), CompletionBuffer: 1, Perf: collector})
	pool.ExecuteActionForTest(ctx, &syncdomain.TrackedAction{
		ID: 1,
		Action: syncdomain.Action{
			Type:    syncdomain.ActionDownload,
			Path:    "remote.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &syncdomain.PathView{
				Path: "remote.txt",
				Remote: &syncdomain.RemoteState{
					DriveID:  remoteDriveID,
					ItemID:   "remote-1",
					ItemType: syncdomain.ItemTypeFile,
					Hash:     "planned",
					Size:     7,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	require.ErrorIs(t, completion.Err, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, 1, collector.Snapshot().SupersededWorkerStartRemoteTruthCount)
}

// Validates: R-2.8.10, R-6.6.17
func TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability(t *testing.T) {
	t.Parallel()

	collector := perf.NewCollector(nil)
	pool := NewWorkerPool(WorkerPoolInputs{Logger: syncfixture.TestLogger(t), CompletionBuffer: 2, Perf: collector})

	pool.sendResult(t.Context(), &syncdomain.TrackedAction{
		ID:     1,
		Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "local.txt"},
	}, &syncdomain.ActionOutcome{
		Action:            syncdomain.ActionUpload,
		Path:              "local.txt",
		Error:             stalePreconditionError("upload source changed"),
		FailureCapability: syncdomain.PermissionCapabilityLocalRead,
	}, syncdomain.ErrActionPreconditionChanged)
	pool.sendResult(t.Context(), &syncdomain.TrackedAction{
		ID:     2,
		Action: syncdomain.Action{Type: syncdomain.ActionRemoteDelete, Path: "remote.txt"},
	}, &syncdomain.ActionOutcome{
		Action:            syncdomain.ActionRemoteDelete,
		Path:              "remote.txt",
		Error:             stalePreconditionError("remote source changed"),
		FailureCapability: syncdomain.PermissionCapabilityRemoteRead,
	}, syncdomain.ErrActionPreconditionChanged)

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
	pool := NewWorkerPool(WorkerPoolInputs{Logger: syncfixture.TestLogger(t), CompletionBuffer: 2, Perf: collector})

	pool.sendResult(t.Context(), &syncdomain.TrackedAction{
		ID:     1,
		Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "local.txt"},
	}, &syncdomain.ActionOutcome{
		Action: syncdomain.ActionUpload,
		Path:   "local.txt",
		Error:  stalePreconditionError("upload source changed"),
	}, syncdomain.ErrActionPreconditionChanged)
	pool.sendResult(t.Context(), &syncdomain.TrackedAction{
		ID:     2,
		Action: syncdomain.Action{Type: syncdomain.ActionDownload, Path: "remote.txt"},
	}, &syncdomain.ActionOutcome{
		Action: syncdomain.ActionDownload,
		Path:   "remote.txt",
		Error:  stalePreconditionError("download target changed"),
	}, syncdomain.ErrActionPreconditionChanged)

	<-pool.Completions()
	<-pool.Completions()

	snapshot := collector.Snapshot()
	assert.Equal(t, 0, snapshot.SupersededLiveLocalPreconditionCount)
	assert.Equal(t, 0, snapshot.SupersededLiveRemotePreconditionCount)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_DownloadDestinationAppearedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "download.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "local-now",
		Size:     9,
	}}))

	pool := NewWorkerPool(WorkerPoolInputs{Store: store, Logger: syncfixture.TestLogger(t), CompletionBuffer: 1})
	pool.ExecuteActionForTest(ctx, &syncdomain.TrackedAction{
		ID: 1,
		Action: syncdomain.Action{
			Type: syncdomain.ActionDownload,
			Path: "download.txt",
			View: &syncdomain.PathView{
				Path: "download.txt",
				Remote: &syncdomain.RemoteState{
					ItemType: syncdomain.ItemTypeFile,
					Hash:     "remote-planned",
					Size:     9,
				},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_LocalDeleteMissingIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, nil))

	pool := NewWorkerPool(WorkerPoolInputs{Store: store, Logger: syncfixture.TestLogger(t), CompletionBuffer: 1})
	pool.ExecuteActionForTest(ctx, &syncdomain.TrackedAction{
		ID: 1,
		Action: syncdomain.Action{
			Type: syncdomain.ActionLocalDelete,
			Path: "delete.txt",
			View: &syncdomain.PathView{
				Path:  "delete.txt",
				Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "planned", Size: 7},
			},
		},
	})

	completion := <-pool.Completions()
	assert.False(t, completion.Success)
	assert.ErrorIs(t, completion.Err, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
//
// The source of a remote-origin local move must still be the synced item.
// Its content may have changed (D-053: the rename carries the edit, which is
// uploaded next), but a different file at the source path (another identity)
// is not the item OneDrive moved.
func TestWorkerStartFreshness_LocalMoveSourceReplacedIsSupersededBeforeExecution(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:             "old.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             "changed",
		Size:             7,
		LocalDevice:      1,
		LocalInode:       99,
		LocalHasIdentity: true,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "remote-1",
		Path:     "new.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "remote",
		Size:     7,
	}}, "delta-1", remoteDriveID))

	pool := NewWorkerPool(WorkerPoolInputs{Store: store, Logger: syncfixture.TestLogger(t), CompletionBuffer: 1})
	pool.ExecuteActionForTest(ctx, &syncdomain.TrackedAction{
		ID: 1,
		Action: syncdomain.Action{
			Type:    syncdomain.ActionLocalMove,
			Path:    "new.txt",
			OldPath: "old.txt",
			ItemID:  "remote-1",
			DriveID: remoteDriveID,
			View: &syncdomain.PathView{
				Path: "new.txt",
				Remote: &syncdomain.RemoteState{
					DriveID:  remoteDriveID,
					ItemID:   "remote-1",
					ItemType: syncdomain.ItemTypeFile,
					Hash:     "remote",
					Size:     7,
				},
				Baseline: &syncdomain.BaselineEntry{
					Path:             "old.txt",
					DriveID:          remoteDriveID,
					ItemID:           "remote-1",
					ItemType:         syncdomain.ItemTypeFile,
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
	assert.ErrorIs(t, completion.Err, syncdomain.ErrActionPreconditionChanged)
}

// Validates: R-2.8.9
func TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "upload.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "planned",
		Size:     7,
	}}))
	require.NoError(t, store.UpsertLocalStateRows(ctx, []syncdomain.LocalStateRow{{
		Path:     "upload.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "changed",
		Size:     7,
	}}))
	require.NoError(t, store.MarkLocalTruthSuspect(ctx, syncdomain.LocalTruthRecoveryDroppedEvents))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "upload.txt",
		View: &syncdomain.PathView{
			Path:  "upload.txt",
			Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "planned", Size: 7},
		},
	})

	require.NoError(t, err)
	assert.True(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestActionFreshness_CanceledContextFailsClosedWithoutStoreRead(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	require.NoError(t, store.Close(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "upload.txt",
		View: &syncdomain.PathView{
			Path:  "upload.txt",
			Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "planned", Size: 7},
		},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestActionFreshness_PostRemoteMoveUploadAllowsMoveProducedETagChange(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &syncdomain.PathView{
			Path: "new.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "old-hash",
				Size:     10,
				Mtime:    1,
				ETag:     "etag-before-move",
			},
			Baseline: &syncdomain.BaselineEntry{
				Path:     "old.txt",
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: syncdomain.ItemTypeFile,
			},
		},
	})

	require.NoError(t, err)
	assert.True(t, decision.Fresh)
}

// Validates: R-2.8.9
func TestActionFreshness_PostRemoteMoveUploadRejectsRemoteContentChange(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	remoteDriveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  remoteDriveID,
		ItemID:   "item-file",
		Path:     "new.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "changed-hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-after-move",
	}}, "delta-1", remoteDriveID))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "new.txt",
		ItemID:  "item-file",
		DriveID: remoteDriveID,
		View: &syncdomain.PathView{
			Path: "new.txt",
			Remote: &syncdomain.RemoteState{
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "old-hash",
				Size:     10,
				Mtime:    1,
				ETag:     "etag-before-move",
			},
			Baseline: &syncdomain.BaselineEntry{
				Path:     "old.txt",
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				ItemType: syncdomain.ItemTypeFile,
			},
		},
	})

	require.NoError(t, err)
	assert.False(t, decision.Fresh)
	assert.Contains(t, decision.Reason, "remote truth changed")
}

// Validates: R-2.8.9, R-2.17.3
//
// The download of OneDrive's edit composed with this computer's rename of the
// item runs after the remote move of the same plan: the move's new eTag is no
// change of the content the download was planned for, and a content change
// still is.
func TestActionFreshness_PostRemoteMoveDownloadAllowsOnlyTheMoveProducedETagChange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		hashNow   string
		wantFresh bool
	}{
		{name: "the move's eTag", hashNow: "edited-hash", wantFresh: true},
		{name: "a later edit", hashNow: "later-hash", wantFresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := syncstoretest.NewTestStore(t)
			ctx := t.Context()
			remoteDriveID := driveid.New(syncfixture.TestDriveID)

			require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
				DriveID:  remoteDriveID,
				ItemID:   "item-file",
				Path:     "new.txt",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     tc.hashNow,
				Size:     10,
				Mtime:    1,
				ETag:     "etag-after-move",
			}}, "delta-1", remoteDriveID))

			decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
				Type:    syncdomain.ActionDownload,
				Path:    "new.txt",
				ItemID:  "item-file",
				DriveID: remoteDriveID,
				View: &syncdomain.PathView{
					Path: "new.txt",
					Remote: &syncdomain.RemoteState{
						DriveID:  remoteDriveID,
						ItemID:   "item-file",
						ItemType: syncdomain.ItemTypeFile,
						Hash:     "edited-hash",
						Size:     10,
						Mtime:    1,
						ETag:     "etag-before-move",
					},
					Baseline: &syncdomain.BaselineEntry{
						Path:     "old.txt",
						DriveID:  remoteDriveID,
						ItemID:   "item-file",
						ItemType: syncdomain.ItemTypeFile,
					},
				},
			})

			require.NoError(t, err)
			assert.Equal(t, tc.wantFresh, decision.Fresh, decision.Reason)
		})
	}
}

// Validates: R-2.8.9, R-2.17.3
//
// The download of OneDrive's edit onto the copy a local move of the same plan
// puts at its path reads the copy where local_state records it, at the
// move's source (MovedLocalPath): the planned copy there is fresh, a copy
// edited there since planning is stale.
func TestActionFreshness_MovedIntoPlaceDownloadReadsTheMoveSource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		hashNow   string
		wantFresh bool
	}{
		{name: "the planned copy", hashNow: "synced", wantFresh: true},
		{name: "an edit since planning", hashNow: "edited here", wantFresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := syncstoretest.NewTestStore(t)
			ctx := t.Context()
			require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
				Path: "old.txt", ItemType: syncdomain.ItemTypeFile, Hash: tc.hashNow, Size: 6,
			}}))

			decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
				Type:           syncdomain.ActionDownload,
				Path:           "new.txt",
				MovedLocalPath: "old.txt",
				View: &syncdomain.PathView{
					Path:  "new.txt",
					Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile, Hash: "synced", Size: 6},
				},
			})

			require.NoError(t, err)
			assert.Equal(t, tc.wantFresh, decision.Fresh, decision.Reason)
		})
	}
}

// Validates: R-2.8.9
func TestActionFreshness_MissingPlannerViewFailsClosedForExecutableAction(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, nil))

	decision, err := EvaluateActionFreshnessFromStore(ctx, store, &syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "upload.txt",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing planner view")
	assert.False(t, decision.Fresh)
}

// Validates: R-2.8.9, R-2.17.3
func TestActionFreshness_LocalOriginMoveRejectsChangedDestination(t *testing.T) {
	t.Parallel()

	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "secret.txt",
		OldPath: "vim.swp",
		ItemID:  "item-swap",
		View:    &syncdomain.PathView{Path: "vim.swp"},
		MovedLocal: &syncdomain.LocalState{
			ItemType: syncdomain.ItemTypeFile, Size: 5, Mtime: 100,
			LocalDevice: 7, LocalInode: 8, LocalHasIdentity: true,
		},
	}
	planned := &syncdomain.LocalStateRow{
		Path: "secret.txt", ItemType: syncdomain.ItemTypeFile, Size: 5, Mtime: 100,
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
	action := &syncdomain.Action{
		Type:    syncdomain.ActionRemoteMove,
		Path:    "new",
		OldPath: "old",
		ItemID:  "item-old",
		View:    &syncdomain.PathView{Path: "old"},
		MovedLocal: &syncdomain.LocalState{
			ItemType: syncdomain.ItemTypeFolder, LocalDevice: 7, LocalInode: 8, LocalBirthTime: born, LocalHasIdentity: true,
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
		destination := &syncdomain.LocalStateRow{
			Path: "new", ItemType: syncdomain.ItemTypeFolder,
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

// Validates: R-6.2.16
//
// The executor's decisions on the action type panic with OutsideTheEnum on a
// value outside the enum, like the vocabulary's
// (TestActionTypeDecisions_PanicOnATypeOutsideTheEnum): never fresh.
func TestExecutorActionTypeDecisions_PanicOnATypeOutsideTheEnum(t *testing.T) {
	t.Parallel()

	const unlisted = syncdomain.ActionCleanup + 1
	outside := func() *syncdomain.Action {
		return &syncdomain.Action{
			Type:    unlisted,
			Path:    "docs/a.txt",
			OldPath: "docs",
			ItemID:  "item-a",
			View:    &syncdomain.PathView{Path: "docs/a.txt", Baseline: &syncdomain.BaselineEntry{Path: "docs", ItemType: syncdomain.ItemTypeFolder}},
		}
	}

	decisions := map[string]func(){
		"evaluateMoveEndpointFreshness": func() { evaluateMoveEndpointFreshness(outside(), actionFreshnessInputs{}) },
		"movePeerPath":                  func() { movePeerPath(outside()) },
	}
	for name, decide := range decisions {
		assert.PanicsWithValue(t, syncdomain.OutsideTheEnum(unlisted), decide, name)
	}
}
