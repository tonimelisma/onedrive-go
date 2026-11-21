package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func TestWatchRuntime_HandleWatchActionCompletion_DrainsPublicationOnlyDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	root := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "sync.txt",
		DriveID: driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before completion")

	p := &watchPipeline{bl: bl}
	err = rt.handleWatchActionCompletion(ctx, p, &syncdomain.ActionCompletion{
		Path:       "sync.txt",
		ItemID:     "sync-item",
		DriveID:    driveID,
		ActionType: syncdomain.ActionDownload,
		Success:    true,
		ActionID:   1,
	})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "publication-only dependents should drain on the engine side")
	assert.Equal(t, 0, rt.sched.graph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately instead of waiting for worker dispatch")
}

func TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializeRuntimeState(&runtimePlan{})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	unlocked := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "after.txt",
		DriveID: driveID,
		ItemID:  "after-item",
		View:    &syncdomain.PathView{Path: "after.txt"},
	}, 2, []int64{1})
	assert.Nil(t, unlocked)

	held := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "held.txt",
		DriveID: driveID,
		ItemID:  "held-item",
	}, 3, nil)
	require.NotNil(t, held)
	rt.holdAction(held, heldReasonRetry, syncdomain.ScopeKey{}, eng.nowFunc().Add(-time.Second))

	outbox, err := rt.runPublicationDrainStage(ctx, bl, []*syncdomain.TrackedAction{publication})
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(2), outbox[0].ID, "publication drain should only enqueue dependents unlocked by publication success")
	assert.Contains(t, rt.retries.heldByKey, syncdomain.RetryWorkKeyForAction(&held.Action), "unrelated held retry work should not be released by publication drain")
}

// Validates: R-2.10.5, R-2.10.33
func TestRunPublicationDrainStage_PublicationSuccessClearsRetryWorkAndAdmitsDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	row := syncdomain.RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   syncdomain.ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Minute).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []syncdomain.RetryWorkRow{row}})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	dependent := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "after.txt",
		DriveID: driveID,
		ItemID:  "after-item",
		View:    &syncdomain.PathView{Path: "after.txt"},
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	outbox, err := rt.runPublicationDrainStage(ctx, bl, []*syncdomain.TrackedAction{publication})
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(2), outbox[0].ID)
	assert.Equal(t, 1, rt.results.succeeded)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))
}

func TestRunPublicationDrainStage_PublicationSuccessDoesNotResetScopeFailureWindows(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializeRuntimeState(&runtimePlan{})
	rt.scopes.state = syncexec.NewScopeState(eng.nowFunc, eng.logger)
	rt.scopes.state.UpdateScope(&syncdomain.ActionCompletion{
		Path:       "service.txt",
		ActionType: syncdomain.ActionUpload,
		DriveID:    driveID,
		HTTPStatus: 503,
		ErrMsg:     "service unavailable",
	})
	require.True(t, rt.scopes.state.HasWindowForTest(syncdomain.SKService()))

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	outbox, err := rt.runPublicationDrainStage(ctx, bl, []*syncdomain.TrackedAction{publication})
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.True(t, rt.scopes.state.HasWindowForTest(syncdomain.SKService()),
		"publication-only successes must not clear unrelated scope failure windows")
}

// Validates: R-2.10.5, R-2.10.33
func TestRunPublicationDrainStage_PersistsRetryWorkOnPublicationCommitFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "seed.txt",
		DriveID:  eng.driveID,
		ItemID:   "seed-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	rt.initializeRuntimeState(&runtimePlan{})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveid.New("0000000000000002"),
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	outbox, err := rt.runPublicationDrainStage(ctx, syncdomain.NewBaselineSnapshot(nil), []*syncdomain.TrackedAction{publication})
	require.NoError(t, err)
	assert.Empty(t, outbox)

	work := syncdomain.RetryWorkKeyForAction(&publication.Action)
	require.Contains(t, rt.retries.heldByKey, work)

	retryRows := listRetryWorkForTest(t, eng.baseline, ctx)
	require.Len(t, retryRows, 1)
	assert.Equal(t, "cleanup.txt", retryRows[0].Path)
	assert.Equal(t, syncdomain.ActionCleanup, retryRows[0].ActionType)
}

// Validates: R-2.10.33
func TestWatchRuntime_HandleWatchHeldRelease_RetryTickReducesReleasedPublicationRetryOnEngineSide(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	row := syncdomain.RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   syncdomain.ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []syncdomain.RetryWorkRow{row}})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))

	err = rt.handleWatchHeldRelease(ctx, &watchPipeline{bl: bl}, false)
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "released publication retries must reduce on the engine side before any worker dispatch")
	assert.Empty(t, rt.retries.heldByKey)
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

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	require.NoError(t, eng.baseline.Close(ctx))

	outbox, err := rt.runPublicationDrainStage(ctx, syncdomain.NewBaselineSnapshot(nil), []*syncdomain.TrackedAction{publication})
	require.Error(t, err)
	require.ErrorContains(t, err, "record retry_work")
	assert.Empty(t, outbox)
	assert.Empty(t, rt.retries.heldByKey)
	assert.Equal(t, 1, rt.sched.graph.InFlightCount(), "publication failure must terminate instead of pretending the unresolved node was handled")
}

// Validates: R-2.8.22
func TestWatchRuntime_ASnapshotTakenBeforeShutdownIsStillCommitted(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rt.handleWatchLocalObservationBatch(ctx, &syncobserve.LocalObservation{
		Kind: syncobserve.LocalSnapshot,
		Findings: []syncdomain.SkippedItem{{
			Path:   "bad?.txt",
			Reason: syncdomain.IssueInvalidFilename,
			Detail: "invalid filename",
		}},
	})
	require.NoError(t, err)

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "bad?.txt", issues[0].Path)
}

func TestWatchRuntime_HandleWatchHeldRelease_CompletesReleasedConcreteActionsOnReductionFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	rt.initializeRuntimeState(&runtimePlan{})

	concrete := rt.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "retry.txt",
		View: &syncdomain.PathView{Path: "retry.txt"},
	}, 1, nil)
	require.NotNil(t, concrete)
	rt.holdAction(concrete, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))

	require.NoError(t, eng.baseline.Close(ctx))

	err := rt.handleWatchHeldRelease(ctx, &watchPipeline{bl: syncdomain.NewBaselineSnapshot(nil)}, false)
	require.ErrorContains(t, err, "reading observation state for action freshness")
	assert.Empty(t, rt.currentOutbox(), "released frontier should be shutdown-completed when reduction fails")
	assert.Equal(t, 1, rt.sched.graph.InFlightCount(), "only the publication action should remain unresolved on fail-closed termination")

	_, concretePresent := rt.sched.graph.Get(1)
	assert.False(t, concretePresent)
	_, publicationPresent := rt.sched.graph.Get(2)
	assert.True(t, publicationPresent)
}
