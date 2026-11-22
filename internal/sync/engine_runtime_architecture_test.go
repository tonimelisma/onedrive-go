package sync

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.33
func TestRuntimeArchitecture_PublicationOnlyActionsNeverReachWorkerOutbox(t *testing.T) {
	t.Parallel()

	type publicationPathCase struct {
		name string
		run  func(t *testing.T, eng *testEngine, rt *watchRuntime, bl syncdomain.BaselineView)
	}

	cases := []publicationPathCase{
		{
			name: "steady-state completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &syncdomain.ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.cfg.driveID,
					ActionType: syncdomain.ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
				assert.Equal(t, 0, rt.sched.graph.InFlightCount())
			},
		},
		{
			name: "bootstrap completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				setWatchPhase(rt, watchPhaseBootstrap)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &syncdomain.ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.cfg.driveID,
					ActionType: syncdomain.ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
				assert.Equal(t, 0, rt.sched.graph.InFlightCount())
			},
		},
		{
			name: "retry tick",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				seedDueRetryPublication(t, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, false)
				require.NoError(t, err)
				assert.Empty(t, rt.retries.heldByKey)
				assert.Empty(t, listRetryWorkForTest(t, rt.deps.store, t.Context()))
			},
		},
		{
			name: "trial tick",
			run: func(t *testing.T, eng *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				seedDueTrialPublication(t, eng, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, true)
				require.NoError(t, err)
				assert.Empty(t, rt.retries.heldByKey)
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

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.cfg.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	rt.replaceOutbox([]*syncdomain.TrackedAction{publication})
	rt.beginWatchDrain(t.Context())

	assert.True(t, rt.isDraining())
	assert.Empty(t, rt.currentOutbox())
	assert.Equal(t, 0, rt.sched.graph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.True(t, found, "shutdown drain should discard publication work instead of committing it through workers")
}

// Validates: R-2.10.5
func TestRuntimeArchitecture_SteadyStatePrepareUsesCommittedTruthOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remoteCalls := 0
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			remoteCalls++
			return nil, errors.New("unexpected remote observation")
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	require.NoError(t, rt.commitObservedItems(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "remote.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash-1",
		Size:     10,
	}}, ""))

	runtime, err := rt.runSteadyStateCurrentPlan(ctx, syncdomain.SyncDownloadOnly)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.Len(t, runtime.Plan.Actions, 1)
	assert.Zero(t, remoteCalls, "steady-state current-plan load must use committed truth instead of observing remote again")
	assert.Equal(t, syncdomain.ActionDownload, runtime.Plan.Actions[0].Type)
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

	current := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "drain.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)

	p := &watchPipeline{
		bl:          bl,
		replanReady: make(chan syncobserve.DirtyBatch),
	}
	rt.resources.localBatches = make(chan syncobserve.LocalObservation)
	rt.resources.remoteBatches = make(chan syncobserve.RemoteObservationBatch)

	cancel()
	rt.beginWatchDrain(ctx)
	require.NoError(t, eng.baseline.Close(context.Background()))

	done, err := rt.handleDrainingCompletion(ctx, p, &syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "drain.txt",
		ActionType: syncdomain.ActionUpload,
		Err:        assert.AnError,
		ErrMsg:     "retry later",
	}, true)
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, rt.isDraining())
	assert.Empty(t, rt.currentOutbox(), "shutdown drain must stay sealed after suppressed completion bookkeeping errors")
	// The phase table, not nil-ed channels, turns the intake off.
	for _, in := range []watchInput{watchInputReplanReady, watchInputObservations} {
		assert.False(t, watchInputOn(rt.phase(), in))
	}
}

// Validates: R-2.10.33
func TestRuntimeArchitecture_WatchPathsShareAppendReadyFrontierBoundary(t *testing.T) {
	t.Parallel()

	type frontierPathCase struct {
		name string
		run  func(t *testing.T, eng *testEngine, rt *watchRuntime, bl syncdomain.BaselineView)
	}

	cases := []frontierPathCase{
		{
			name: "steady-state completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &syncdomain.ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.cfg.driveID,
					ActionType: syncdomain.ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "bootstrap completion",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				addPublicationDependencyPair(t, rt)
				setWatchPhase(rt, watchPhaseBootstrap)
				err := rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &syncdomain.ActionCompletion{
					Path:       "sync.txt",
					ItemID:     "sync-item",
					DriveID:    rt.cfg.driveID,
					ActionType: syncdomain.ActionDownload,
					Success:    true,
					ActionID:   1,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "retry tick",
			run: func(t *testing.T, _ *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
				t.Helper()
				seedDueRetryPublication(t, rt)
				err := rt.handleWatchHeldRelease(t.Context(), &watchPipeline{bl: bl}, false)
				require.NoError(t, err)
			},
		},
		{
			name: "trial tick",
			run: func(t *testing.T, eng *testEngine, rt *watchRuntime, bl syncdomain.BaselineView) {
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

			recorder.RequireEventCount(t, func(event syncdomain.EngineDebugEvent) bool {
				return event.Type == syncdomain.EngineDebugEventReadyFrontierAppended
			}, 1, "watch path should re-enter the shared appendReadyFrontier boundary exactly once")
		})
	}
}

func seedPublicationBaseline(t *testing.T, eng *testEngine) syncdomain.BaselineView {
	t.Helper()

	require.NoError(t, eng.baseline.CommitMutation(t.Context(), &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	return bl
}

func addPublicationDependencyPair(t *testing.T, rt *watchRuntime) {
	t.Helper()

	root := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "sync.txt",
		DriveID: rt.cfg.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.cfg.driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before completion")
}

func seedDueRetryPublication(t *testing.T, rt *watchRuntime) {
	t.Helper()

	now := rt.deps.now()
	row := syncdomain.RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   syncdomain.ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, rt.deps.store.UpsertRetryWork(t.Context(), &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []syncdomain.RetryWorkRow{row}})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.cfg.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))
}

func seedDueTrialPublication(t *testing.T, eng *testEngine, rt *watchRuntime) {
	t.Helper()

	scopeKey := syncdomain.SKService()
	now := rt.deps.now()
	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: rt.cfg.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonScope, scopeKey, time.Time{})
}

// No run holds the engine: neither the flow, nor the runtimes that embed it,
// nor the observation the off-loop goroutines hold, nor any owner they reach
// by value or pointer has a field of type *Engine. What a run reads of the
// engine is what flowInputs handed it by name.
//
// Validates: R-6.10.29
func TestRuntimeArchitecture_NoRunStateHoldsTheEngine(t *testing.T) {
	t.Parallel()

	engineType := reflect.TypeFor[*Engine]()
	pkgPath := reflect.TypeFor[engineFlow]().PkgPath()
	seen := make(map[reflect.Type]bool)
	var found []string
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ.PkgPath() != pkgPath || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.Type == engineType {
				found = append(found, path+"."+field.Name)
				continue
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	for _, root := range []reflect.Type{
		reflect.TypeFor[watchRuntime](),
		reflect.TypeFor[oneShotRunner](),
		reflect.TypeFor[engineFlow](),
		reflect.TypeFor[observation](),
		reflect.TypeFor[flowInputs](),
	} {
		walk(root, root.Name())
	}

	assert.Empty(t, found, "run state that holds the engine")
	assert.True(t, seen[reflect.TypeFor[shortcutRoots]()], "the walk reaches the shortcut roots")
	assert.True(t, seen[reflect.TypeFor[remoteSource]()], "the walk reaches the remote source")
}
