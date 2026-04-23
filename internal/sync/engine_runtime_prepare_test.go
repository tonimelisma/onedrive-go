package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func assertRuntimePlanEqual(t *testing.T, expected *runtimePlan, actual *runtimePlan) {
	t.Helper()

	require.NotNil(t, expected)
	require.NotNil(t, actual)
	assert.Equal(t, expected.PendingCursorCommit, actual.PendingCursorCommit)
	assert.Equal(t, expected.Report, actual.Report)
	assert.Equal(t, expected.Plan, actual.Plan)
	assert.Equal(t, expected.RetryRows, actual.RetryRows)
	assert.Equal(t, expected.BlockScopes, actual.BlockScopes)
}

func seedStaleRetryAndBlockScopeForPrepareTest(t *testing.T, ctx context.Context, eng *testEngine) {
	t.Helper()

	_, err := eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          "stale.txt",
		ActionType:    ActionUpload,
		ConditionType: IssueServiceOutage,
		LastError:     "stale retry row",
	}, func(int) time.Duration { return time.Minute })
	require.NoError(t, err)
	require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		BlockedAt:     eng.nowFunc(),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))
}

// Validates: R-2.10.5
func TestRunBootstrapCurrentPlan_MatchesOneShotLiveCurrentPlan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash-1",
				},
			}, "token-prepare"), nil
		},
	}

	oneShotEng, _ := newTestEngine(t, mock)
	watchEng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	oneShotBaseline, err := oneShotEng.baseline.Load(ctx)
	require.NoError(t, err)
	fullReconcile, err := oneShotEng.shouldRunFullRemoteRefresh(ctx, false)
	require.NoError(t, err)
	oneShotPrepared, err := newOneShotRunner(oneShotEng.Engine).runLiveCurrentPlan(ctx, oneShotBaseline, SyncDownloadOnly, RunOptions{
		FullReconcile: fullReconcile,
	})
	require.NoError(t, err)

	setupWatchEngine(t, watchEng)
	watchBaseline, err := watchEng.baseline.Load(ctx)
	require.NoError(t, err)
	watchPrepared, err := testWatchRuntime(t, watchEng).runBootstrapCurrentPlan(ctx, watchBaseline, SyncDownloadOnly)
	require.NoError(t, err)

	assertRuntimePlanEqual(t, oneShotPrepared, watchPrepared)
}

// Validates: R-2.10.5
func TestRunStartupStage_OneShotAndWatchShareStartupNormalization(t *testing.T) {
	t.Parallel()

	oneShotEng, _ := newTestEngine(t, &engineMockClient{})
	watchEng, _ := newTestEngine(t, &engineMockClient{})
	ctx := t.Context()

	require.NoError(t, oneShotEng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		BlockedAt:     oneShotEng.nowFunc(),
		TrialInterval: time.Minute,
		NextTrialAt:   oneShotEng.nowFunc().Add(time.Minute),
	}))
	require.NoError(t, watchEng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		BlockedAt:     watchEng.nowFunc(),
		TrialInterval: time.Minute,
		NextTrialAt:   watchEng.nowFunc().Add(time.Minute),
	}))

	oneShotBaseline, err := newOneShotRunner(oneShotEng.Engine).runStartupStage(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, oneShotBaseline)

	setupWatchEngine(t, watchEng)
	watchBaseline, err := testWatchRuntime(t, watchEng).runStartupStage(ctx, testWatchRuntime(t, watchEng))
	require.NoError(t, err)
	require.NotNil(t, watchBaseline)

	oneShotScopes, err := oneShotEng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, oneShotScopes)

	watchScopes, err := watchEng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, watchScopes)
}

// Validates: R-2.10.5
func TestRunLiveCurrentPlan_FailsClosedWhenRemoteObservationReconcileFails(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash-1",
				},
			}, "token-prepare"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	var closeStore sync.Once
	attachDebugEventRecorderWithHook(eng, func(event engineDebugEvent) {
		if event.Type != engineDebugEventObservationFindingsReconcileStarted || event.Note != engineDebugNoteRemoteCurrent {
			return
		}
		closeStore.Do(func() {
			require.NoError(t, eng.baseline.Close(context.Background()))
		})
	})

	fullReconcile, err := eng.shouldRunFullRemoteRefresh(ctx, false)
	require.NoError(t, err)
	_, err = newOneShotRunner(eng.Engine).runLiveCurrentPlan(ctx, bl, SyncDownloadOnly, RunOptions{
		FullReconcile: fullReconcile,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to reconcile remote observation findings")
}

// Validates: R-2.10.5
func TestBootstrapSync_FailsClosedWhenRemoteObservationReconcileFails(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash-1",
				},
			}, "token-bootstrap"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	bl, err := rt.runStartupStage(ctx, rt)
	require.NoError(t, err)
	var closeStore sync.Once
	attachDebugEventRecorderWithHook(eng, func(event engineDebugEvent) {
		if event.Type != engineDebugEventObservationFindingsReconcileStarted || event.Note != engineDebugNoteRemoteCurrent {
			return
		}
		closeStore.Do(func() {
			require.NoError(t, eng.baseline.Close(context.Background()))
		})
	})

	err = rt.bootstrapSync(ctx, SyncDownloadOnly, &watchPipeline{
		bl:          bl,
		completions: make(chan ActionCompletion),
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to reconcile remote observation findings")
}

// Validates: R-2.10.5
func TestBootstrapSync_RequiresStartupPreparedBaseline(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)

	err := testWatchRuntime(t, eng).bootstrapSync(t.Context(), SyncDownloadOnly, &watchPipeline{
		completions: make(chan ActionCompletion),
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "bootstrap requires startup baseline")
}

// Validates: R-2.10.5
func TestRunSteadyStateReplan_PrunesStaleDurableRuntimeStateLikeBootstrapPrepare(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-bootstrap"), nil
		},
	}

	bootstrapEng, _ := newTestEngine(t, mock)
	dirtyEng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	seedStaleRetryAndBlockScopeForPrepareTest(t, ctx, bootstrapEng)
	seedStaleRetryAndBlockScopeForPrepareTest(t, ctx, dirtyEng)

	setupWatchEngine(t, bootstrapEng)
	bootstrapBaseline, err := bootstrapEng.baseline.Load(ctx)
	require.NoError(t, err)
	bootstrapPrepared, err := testWatchRuntime(t, bootstrapEng).runBootstrapCurrentPlan(ctx, bootstrapBaseline, SyncBidirectional)
	require.NoError(t, err)
	assert.Empty(t, bootstrapPrepared.RetryRows)
	assert.Empty(t, bootstrapPrepared.BlockScopes)

	setupWatchEngine(t, dirtyEng)
	dirtyBaseline, err := dirtyEng.baseline.Load(ctx)
	require.NoError(t, err)
	rt := testWatchRuntime(t, dirtyEng)
	err = rt.runSteadyStateReplan(ctx, &watchPipeline{
		bl:   dirtyBaseline,
		mode: SyncBidirectional,
	}, dirtyBatch{})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox())

	dirtyRetryRows, err := dirtyEng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, dirtyRetryRows)

	dirtyBlockScopes, err := dirtyEng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, dirtyBlockScopes)
}

// Validates: R-2.10.5
func TestReconcileRuntimeStateStage_PrunesAndLoadsDurableRuntimeState(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-runtime-prepare"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	seedStaleRetryAndBlockScopeForPrepareTest(t, ctx, eng)

	setupWatchEngine(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt := testWatchRuntime(t, eng)
	observed, err := rt.loadObservedCurrentInputs(ctx, nil)
	require.NoError(t, err)

	build, err := rt.buildCurrentPlanStage(ctx, bl, SyncBidirectional, RunOptions{}, observed)
	require.NoError(t, err)

	runtime, err := rt.reconcileRuntimeStateStage(ctx, build)
	require.NoError(t, err)

	assert.Empty(t, runtime.RetryRows)
	assert.Empty(t, runtime.BlockScopes)
}

// Validates: R-2.10.5
func TestKeepCurrentPlanBuild_LeavesDurableRuntimeStateUntouched(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-dry-runtime-prepare"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	seedStaleRetryAndBlockScopeForPrepareTest(t, ctx, eng)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	runner := newOneShotRunner(eng.Engine)
	observed, err := runner.observeDryRunCurrentState(ctx, bl, false)
	require.NoError(t, err)

	build, err := runner.buildCurrentPlanStage(ctx, bl, SyncBidirectional, RunOptions{DryRun: true}, observed)
	require.NoError(t, err)

	runtime := runner.keepBuiltCurrentPlan(build)
	require.NotNil(t, runtime)
	assert.Empty(t, runtime.RetryRows)
	assert.Empty(t, runtime.BlockScopes)

	retryRows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retryRows, 1)
	assert.Equal(t, "stale.txt", retryRows[0].Path)

	blockScopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, SKService(), blockScopes[0].Key)
}
