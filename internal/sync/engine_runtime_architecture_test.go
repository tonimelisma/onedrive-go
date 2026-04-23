package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.10.33
func TestRuntimeArchitecture_PublicationOnlyActionsNeverReachWorkerOutbox(t *testing.T) {
	t.Parallel()

	type publicationPathCase struct {
		name string
		run  func(t *testing.T, eng *testEngine, rt *watchRuntime, bl *Baseline)
	}

	cases := []publicationPathCase{
		{
			name: "steady-state completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.engine.driveID,
					ActionType: ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
				assert.Equal(t, 0, rt.depGraph.InFlightCount())
			},
		},
		{
			name: "bootstrap completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				rt.enterBootstrap()
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.engine.driveID,
					ActionType: ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
				assert.Equal(t, 0, rt.depGraph.InFlightCount())
			},
		},
		{
			name: "retry tick",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				seedDueRetryPublication(t, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, false)
				require.NoError(t, err)
				assert.Empty(t, rt.heldByKey)
				assert.Empty(t, listRetryWorkForTest(t, rt.engine.baseline, t.Context()))
			},
		},
		{
			name: "trial tick",
			run: func(t *testing.T, eng *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				seedDueTrialPublication(t, eng, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, true)
				require.NoError(t, err)
				assert.Empty(t, rt.heldByKey)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			rt := testWatchRuntime(t, eng)
			bl := seedPublicationBaseline(t, eng)

			tc.run(t, eng, rt, bl)

			assert.Empty(t, rt.currentOutbox(), "publication-only actions must never reach the worker outbox")
		})
	}
}

// Validates: R-2.10.33, R-6.10.10
func TestRuntimeArchitecture_ShutdownDrainDoesNotRoutePublicationWorkToWorkers(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	bl := seedPublicationBaseline(t, eng)

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.engine.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	rt.replaceOutbox([]*TrackedAction{publication})
	p := &watchPipeline{bl: bl}
	rt.beginWatchDrain(t.Context(), p)

	assert.True(t, rt.isDraining())
	assert.Empty(t, rt.currentOutbox())
	assert.Equal(t, 0, rt.depGraph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.True(t, found, "shutdown drain should discard publication work instead of committing it through workers")
}

// Validates: R-2.10.5
func TestRuntimeArchitecture_SteadyStatePrepareUsesCommittedTruthOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remoteCalls := 0
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			remoteCalls++
			return nil, errors.New("unexpected remote observation")
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	require.NoError(t, rt.commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "remote.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-1",
		Size:     10,
	}}, ""))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	runtime, err := rt.runSteadyStateCurrentPlan(ctx, bl, SyncDownloadOnly)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.Len(t, runtime.Plan.Actions, 1)
	assert.Zero(t, remoteCalls, "steady-state current-plan load must use committed truth instead of observing remote again")
	assert.Equal(t, ActionDownload, runtime.Plan.Actions[0].Type)
	assert.Equal(t, "remote.txt", runtime.Plan.Actions[0].Path)
}

// Validates: R-6.10.10
func TestRuntimeArchitecture_ShutdownDrainSealsAdmissionOnCompletionError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "drain.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)

	p := &watchPipeline{
		bl:          bl,
		replanReady: make(chan dirtyBatch),
	}
	rt.localEvents = make(chan ChangeEvent)
	rt.remoteBatches = make(chan remoteObservationBatch)

	cancel()
	rt.beginWatchDrain(ctx, p)
	require.NoError(t, eng.baseline.Close(context.Background()))

	done, err := rt.handleDrainingCompletion(ctx, p, &ActionCompletion{
		ActionID:   1,
		Path:       "drain.txt",
		ActionType: ActionUpload,
		Err:        assert.AnError,
		ErrMsg:     "retry later",
	}, true)
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, rt.isDraining())
	assert.Empty(t, rt.currentOutbox(), "shutdown drain must stay sealed after suppressed completion bookkeeping errors")
	assert.Nil(t, p.replanReady)
	assert.Nil(t, rt.localEvents)
	assert.Nil(t, rt.remoteBatches)
}

// Validates: R-2.10.33
func TestRuntimeArchitecture_WatchPathsShareAppendReadyFrontierBoundary(t *testing.T) {
	t.Parallel()

	type frontierPathCase struct {
		name string
		run  func(t *testing.T, eng *testEngine, rt *watchRuntime, bl *Baseline)
	}

	cases := []frontierPathCase{
		{
			name: "steady-state completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.engine.driveID,
					ActionType: ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "bootstrap completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				rt.enterBootstrap()
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.engine.driveID,
					ActionType: ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "retry tick",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				seedDueRetryPublication(t, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, false)
				require.NoError(t, err)
			},
		},
		{
			name: "trial tick",
			run: func(t *testing.T, eng *testEngine, rt *watchRuntime, bl *Baseline) {
				t.Helper()
				seedDueTrialPublication(t, eng, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, true)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			recorder := attachDebugEventRecorder(eng)
			rt := testWatchRuntime(t, eng)
			bl := seedPublicationBaseline(t, eng)

			tc.run(t, eng, rt, bl)

			recorder.requireEventCount(t, func(event engineDebugEvent) bool {
				return event.Type == engineDebugEventReadyFrontierAppended
			}, 1, "watch path should re-enter the shared appendReadyFrontier boundary exactly once")
		})
	}
}

func seedPublicationBaseline(t *testing.T, eng *testEngine) *Baseline {
	t.Helper()

	require.NoError(t, eng.baseline.CommitMutation(t.Context(), &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	return bl
}

func addPublicationDependencyPair(t *testing.T, rt *watchRuntime) {
	t.Helper()

	root := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "sync.txt",
		DriveID: rt.engine.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.engine.driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before completion")
}

func seedDueRetryPublication(t *testing.T, rt *watchRuntime) {
	t.Helper()

	now := rt.engine.nowFunc()
	row := RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, rt.engine.baseline.UpsertRetryWork(t.Context(), &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []RetryWorkRow{row}})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.engine.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))
}

func seedDueTrialPublication(t *testing.T, eng *testEngine, rt *watchRuntime) {
	t.Helper()

	scopeKey := SKService()
	now := rt.engine.nowFunc()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.engine.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonScope, scopeKey, time.Time{})
}
