package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestWatchRuntime_HandleWatchActionCompletion_DrainsPublicationOnlyDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	root := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "sync.txt",
		DriveID: driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before completion")

	p := &watchPipeline{bl: bl}
	err = rt.handleWatchActionCompletion(ctx, p, &ActionCompletion{
		Path:       "sync.txt",
		ItemID:     "sync-item",
		DriveID:    driveID,
		ActionType: ActionDownload,
		Success:    true,
		ActionID:   1,
	})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "publication-only dependents should drain on the engine side")
	assert.Equal(t, 0, rt.depGraph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately instead of waiting for worker dispatch")
}

func TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializeRuntimeState(&runtimePlan{})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	unlocked := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "after.txt",
		DriveID: driveID,
		ItemID:  "after-item",
		View:    &PathView{Path: "after.txt"},
	}, 2, []int64{1})
	assert.Nil(t, unlocked)

	held := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "held.txt",
		DriveID: driveID,
		ItemID:  "held-item",
	}, 3, nil)
	require.NotNil(t, held)
	rt.holdAction(held, heldReasonRetry, ScopeKey{}, eng.nowFunc().Add(-time.Second))

	outbox, err := rt.runPublicationDrainStage(ctx, rt, bl, []*TrackedAction{publication})
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(2), outbox[0].ID, "publication drain should only enqueue dependents unlocked by publication success")
	assert.Contains(t, rt.heldByKey, retryWorkKeyForAction(&held.Action), "unrelated held retry work should not be released by publication drain")
}

// Validates: R-2.10.5, R-2.10.33
func TestRunPublicationDrainStage_PublicationSuccessClearsRetryWorkAndAdmitsDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFn()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	row := RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Minute).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []RetryWorkRow{row}})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "after.txt",
		DriveID: driveID,
		ItemID:  "after-item",
		View:    &PathView{Path: "after.txt"},
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	outbox, err := rt.runPublicationDrainStage(ctx, rt, bl, []*TrackedAction{publication})
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(2), outbox[0].ID)
	assert.Equal(t, 1, rt.succeeded)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
}

func TestRunPublicationDrainStage_PublicationSuccessDoesNotResetScopeFailureWindows(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializeRuntimeState(&runtimePlan{})
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	rt.scopeState.UpdateScope(&ActionCompletion{
		Path:       "service.txt",
		ActionType: ActionUpload,
		DriveID:    driveID,
		HTTPStatus: 503,
		ErrMsg:     "service unavailable",
	})
	require.Contains(t, rt.scopeState.windows, SKService())

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	outbox, err := rt.runPublicationDrainStage(ctx, rt, bl, []*TrackedAction{publication})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.Contains(t, rt.scopeState.windows, SKService(),
		"publication-only successes must not clear unrelated scope failure windows")
}

// Validates: R-2.10.5, R-2.10.33
func TestRunPublicationDrainStage_PersistsRetryWorkOnPublicationCommitFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "seed.txt",
		DriveID:  eng.driveID,
		ItemID:   "seed-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	rt.initializeRuntimeState(&runtimePlan{})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveid.New("0000000000000002"),
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	outbox, err := rt.runPublicationDrainStage(ctx, rt, &Baseline{}, []*TrackedAction{publication})
	require.NoError(t, err)
	assert.Empty(t, outbox)

	work := retryWorkKeyForAction(&publication.Action)
	require.Contains(t, rt.heldByKey, work)

	retryRows := listRetryWorkForTest(t, eng.baseline, ctx)
	require.Len(t, retryRows, 1)
	assert.Equal(t, "cleanup.txt", retryRows[0].Path)
	assert.Equal(t, ActionCleanup, retryRows[0].ActionType)
}

// Validates: R-2.10.33
func TestWatchRuntime_HandleWatchHeldRelease_RetryTickReducesReleasedPublicationRetryOnEngineSide(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	row := RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []RetryWorkRow{row}})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	err = rt.handleWatchHeldRelease(ctx, &watchPipeline{bl: bl}, false)
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "released publication retries must reduce on the engine side before any worker dispatch")
	assert.Empty(t, rt.heldByKey)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit during retry release instead of reaching workers")
}

// Validates: R-6.8
func TestRunPublicationDrainStage_TerminatesWhenPublicationRetryPersistenceFails(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	rt.initializeRuntimeState(&runtimePlan{})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	require.NoError(t, eng.baseline.Close(ctx))

	outbox, err := rt.runPublicationDrainStage(ctx, rt, &Baseline{}, []*TrackedAction{publication})
	require.Error(t, err)
	require.ErrorContains(t, err, "record retry_work")
	assert.Empty(t, outbox)
	assert.Empty(t, rt.heldByKey)
	assert.Equal(t, 1, rt.depGraph.InFlightCount(), "publication failure must terminate instead of pretending the unresolved node was handled")
}

func TestWatchRuntime_HandleWatchSkippedSignal_ShutdownCancellationIsNonFatal(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done, err := rt.handleWatchSkippedSignal(ctx, []SkippedItem{{
		Path:   "bad?.txt",
		Reason: IssueInvalidFilename,
		Detail: "invalid filename",
	}}, true)

	require.NoError(t, err)
	assert.False(t, done)
}

func TestWatchRuntime_HandleWatchHeldRelease_CompletesReleasedConcreteActionsOnReductionFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	rt.initializeRuntimeState(&runtimePlan{})

	concrete := rt.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "retry.txt",
		View: &PathView{Path: "retry.txt"},
	}, 1, nil)
	require.NotNil(t, concrete)
	rt.holdAction(concrete, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))

	require.NoError(t, eng.baseline.Close(ctx))

	err := rt.handleWatchHeldRelease(ctx, &watchPipeline{bl: &Baseline{}}, false)
	require.ErrorContains(t, err, "reading observation state for action freshness")
	assert.Empty(t, rt.currentOutbox(), "released frontier should be shutdown-completed when reduction fails")
	assert.Equal(t, 1, rt.depGraph.InFlightCount(), "only the publication action should remain unresolved on fail-closed termination")

	_, concretePresent := rt.depGraph.Get(1)
	assert.False(t, concretePresent)
	_, publicationPresent := rt.depGraph.Get(2)
	assert.True(t, publicationPresent)
}
