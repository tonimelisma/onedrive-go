package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.4.5
func TestObservationSessionPlan_DefaultRootDelta(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	session, err := flow.BuildObservationSession(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), session.Generation)

	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:       &session,
		SyncMode:      SyncBidirectional,
		FullReconcile: true,
		Purpose:       observationPlanPurposeOneShot,
	})
	require.NoError(t, err)

	assert.Equal(t, observationPhaseDriverRootDelta, plan.PrimaryPhase.Driver)
	assert.Equal(t, observationPhaseDispatchPolicySingleBatch, plan.PrimaryPhase.DispatchPolicy)
	assert.Equal(t, observationPhaseErrorPolicyFailBatch, plan.PrimaryPhase.ErrorPolicy)
	assert.Equal(t, observationPhaseFallbackPolicyNone, plan.PrimaryPhase.FallbackPolicy)
	assert.Equal(t, observationPhaseTokenCommitPolicyAfterPlannerAccepts, plan.PrimaryPhase.TokenCommitPolicy)
	assert.False(t, plan.PrimaryPhase.HasTargets())
	assert.Equal(t, ScopeReconcileNone, plan.Reentry.Kind)
	assert.Len(t, plan.Hash, 64)
	assert.Equal(t, ScopeObservationRootDelta, flow.scopeObservationMode(&plan))
	assert.True(t, effectivePrimaryFullReconcile(true, &plan))
	require.NoError(t, flow.validatePrimaryObservationPersistence(&plan))
}

// Validates: R-2.4.5, R-2.8.5
func TestObservationSessionPlan_ScopedRootAndHashInputs(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.rootItemID = "shared-root"
	eng.enableWebsocket = true
	flow := testEngineFlow(t, eng)

	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session: &ObservationSession{Generation: 7},
		Purpose: observationPlanPurposeWatch,
	})
	require.NoError(t, err)

	assert.Equal(t, observationPhaseDriverScopedRoot, plan.PrimaryPhase.Driver)
	assert.Equal(t, observationPhaseFallbackPolicyDeltaToEnumerate, plan.PrimaryPhase.FallbackPolicy)
	assert.Equal(t, ScopeObservationScopedDelta, flow.scopeObservationMode(&plan))
	assert.False(t, flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase))

	rootDelta := ObservationSessionPlan{
		PrimaryPhase: ObservationPhasePlan{
			Driver:            observationPhaseDriverRootDelta,
			DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
			ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
			FallbackPolicy:    observationPhaseFallbackPolicyNone,
			TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
		},
		Reentry: ReentryPlan{Kind: ScopeReconcileNone},
	}
	hashWithWebsocket, err := flow.planHash(&rootDelta, false)
	require.NoError(t, err)

	eng.enableWebsocket = false
	hashWithoutWebsocket, err := flow.planHash(&rootDelta, false)
	require.NoError(t, err)

	assert.NotEqual(t, hashWithWebsocket, hashWithoutWebsocket)

	targetPlan := ObservationSessionPlan{
		PrimaryPhase: ObservationPhasePlan{
			Driver: observationPhaseDriverScopedTarget,
			Targets: []plannedObservationTarget{{
				scopeID:   "folder-1",
				localPath: "docs",
				mode:      primaryObservationEnumerate,
			}},
		},
	}
	assert.True(t, targetPlan.PrimaryPhase.HasTargets())
	assert.Equal(t, ScopeObservationScopedEnumerate, flow.scopeObservationMode(&targetPlan))
	assert.Equal(t, ScopeObservationScopedDelta, scopeObservationModeForPrimary(primaryObservationDelta))
	assert.Equal(t, ScopeObservationScopedEnumerate, scopeObservationModeForPrimary(primaryObservationEnumerate))
	assert.Panics(t, func() {
		scopeObservationModeForPrimary(primaryObservationMode("broken"))
	})
}

// Validates: R-2.4.5
func TestBuildObservationSessionPlan_RequiresSession(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	_, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing session")
}

// Validates: R-2.4.5
func TestValidateObservationPlanContracts(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, validateObservationSessionPlan(nil, true), "required")

	err := validateObservationPhasePlan(ObservationPhasePlan{
		Targets: []plannedObservationTarget{{scopeID: "folder-1"}},
	})
	require.ErrorContains(t, err, "empty driver")

	err = validateObservationPhasePlan(ObservationPhasePlan{
		Driver:            observationPhaseDriverScopedTarget,
		DispatchPolicy:    observationPhaseDispatchPolicyParallelTargets,
		ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
		FallbackPolicy:    observationPhaseFallbackPolicyDeltaToEnumerate,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	})
	require.NoError(t, err)

	err = validatePrimaryObservationPhase(ObservationPhasePlan{
		Driver:            observationPhaseDriverScopedRoot,
		DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
		ErrorPolicy:       observationPhaseErrorPolicyIsolateTarget,
		FallbackPolicy:    observationPhaseFallbackPolicyDeltaToEnumerate,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	})
	require.ErrorContains(t, err, "fail_batch")

	err = validatePrimaryObservationPhase(ObservationPhasePlan{
		Driver:            observationPhaseDriverScopedRoot,
		DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
		ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
		FallbackPolicy:    ObservationPhaseFallbackPolicy("broken"),
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	})
	require.ErrorContains(t, err, "invalid fallback policy")

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	err = flow.validatePrimaryObservationPersistence(&ObservationSessionPlan{
		PrimaryPhase: ObservationPhasePlan{
			Driver:            observationPhaseDriverRootDelta,
			DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
			ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
			FallbackPolicy:    observationPhaseFallbackPolicyNone,
			TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
		},
	})
	require.ErrorContains(t, err, "non-empty plan hash")
}

// Validates: R-2.4.5
func TestObservationPhaseHelpersAndDeferredTokenCommit(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	bl := emptyBaseline()

	result, err := flow.executeObservationPhase(t.Context(), bl, ObservationPhasePlan{}, false)
	require.NoError(t, err)
	assert.Empty(t, result.events)

	_, err = flow.observeObservationPhase(t.Context(), bl, ObservationPhasePlan{
		Driver:         observationPhaseDriverRootDelta,
		DispatchPolicy: ObservationPhaseDispatchPolicy("broken"),
	}, false)
	require.ErrorContains(t, err, "unknown dispatch policy")

	_, err = flow.executeSingleBatchObservationPhase(t.Context(), bl, ObservationPhasePlan{
		Driver: observationPhaseDriverScopedTarget,
	}, false)
	require.ErrorContains(t, err, "requires target dispatch")

	_, err = flow.executeSequentialTargetObservationPhase(t.Context(), bl, ObservationPhasePlan{
		Driver: observationPhaseDriverRootDelta,
	}, false)
	require.ErrorContains(t, err, "requires scoped target driver")

	parallelResult, err := flow.executeParallelTargetObservationPhase(t.Context(), bl, ObservationPhasePlan{
		Driver:         observationPhaseDriverScopedTarget,
		DispatchPolicy: observationPhaseDispatchPolicyParallelTargets,
	}, false)
	require.NoError(t, err)
	assert.Empty(t, parallelResult.events)

	kept, err := flow.finalizeObservationPhase(t.Context(), ObservationPhasePlan{
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	}, remoteFetchResult{
		deferred: []deferredDeltaToken{{token: "ignored"}},
	})
	require.NoError(t, err)
	require.Len(t, kept.deferred, 1)

	committed, err := flow.finalizeObservationPhase(t.Context(), ObservationPhasePlan{
		Driver:            observationPhaseDriverRootDelta,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPhaseSuccess,
	}, remoteFetchResult{
		deferred: []deferredDeltaToken{{
			driveID: eng.driveID.String(),
			token:   "token-after-phase",
		}},
	})
	require.NoError(t, err)
	assert.Empty(t, committed.deferred)

	savedToken, err := eng.baseline.GetDeltaToken(t.Context(), eng.driveID.String(), "")
	require.NoError(t, err)
	assert.Equal(t, "token-after-phase", savedToken)

	_, err = flow.finalizeObservationPhase(t.Context(), ObservationPhasePlan{
		TokenCommitPolicy: ObservationPhaseTokenCommitPolicy("broken"),
	}, remoteFetchResult{})
	require.ErrorContains(t, err, "unknown token commit policy")

	assert.Equal(t, "docs/report.txt", observationTargetLogID(plannedObservationTarget{localPath: "docs/report.txt"}))
	assert.Equal(t, "scope-1", observationTargetLogID(plannedObservationTarget{scopeID: "scope-1"}))
}

// Validates: R-2.4.5, R-2.8.2
func TestObservationPostprocessHelpers(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("drive-1")
	projected := projectRemoteObservations(testLogger(t), []ChangeEvent{
		{
			Source:   SourceRemote,
			DriveID:  remoteDriveID,
			ItemID:   "remote-1",
			ParentID: "root",
			Path:     "docs/report.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-1",
			Size:     42,
			Mtime:    99,
			ETag:     "etag-1",
		},
		{
			Source:   SourceLocal,
			Path:     "local.txt",
			ItemType: ItemTypeFile,
		},
		{
			Source:   SourceRemote,
			Path:     "missing-id.txt",
			ItemType: ItemTypeFile,
		},
	})
	require.Len(t, projected.observed, 1)
	require.Len(t, projected.emitted, 3)
	assert.Equal(t, "remote-1", projected.observed[0].ItemID)
	assert.Equal(t, "local.txt", projected.emitted[1].Path)
	assert.Equal(t, "missing-id.txt", projected.emitted[2].Path)

	wrapped := newFatalObserverError(errors.New("boom"))
	require.Error(t, wrapped)
	assert.True(t, isFatalObserverError(wrapped))
	assert.False(t, isFatalObserverError(errors.New("plain")))
	require.NoError(t, newFatalObserverError(nil))

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	primaryEvents := []ChangeEvent{{
		Source:   SourceRemote,
		DriveID:  eng.driveID,
		ItemID:   "item-primary",
		ParentID: "root",
		Path:     "docs/primary.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-primary",
	}}
	finalEvents, err := rt.processCommittedPrimaryWatchBatch(t.Context(), emptyBaseline(), primaryEvents, "primary-token")
	require.NoError(t, err)
	assert.Equal(t, primaryEvents, finalEvents)

	row, found, err := eng.baseline.GetRemoteStateByID(t.Context(), eng.driveID, "item-primary")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "docs/primary.txt", row.Path)

	scopedEvents, committed := rt.processCommittedScopedWatchBatch(t.Context(), emptyBaseline(), remoteFetchResult{
		events: []ChangeEvent{{
			Source:   SourceRemote,
			DriveID:  eng.driveID,
			ItemID:   "item-scoped",
			ParentID: "root",
			Path:     "docs/scoped.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-scoped",
		}},
		deferred: []deferredDeltaToken{{
			driveID: eng.driveID.String(),
			token:   "scoped-token",
		}},
	})
	require.True(t, committed)
	assert.Len(t, scopedEvents, 1)

	scopedToken, err := eng.baseline.GetDeltaToken(t.Context(), eng.driveID.String(), "")
	require.NoError(t, err)
	assert.Equal(t, "scoped-token", scopedToken)

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	nilEvents, committed := rt.processCommittedScopedWatchBatch(canceledCtx, emptyBaseline(), remoteFetchResult{
		events: primaryEvents,
	})
	assert.False(t, committed)
	assert.Nil(t, nilEvents)

	nilEvents, err = rt.processCommittedPrimaryWatchBatch(canceledCtx, emptyBaseline(), primaryEvents, "ignored-token")
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, nilEvents)

	copied := rt.processCommittedPrimaryBatch(t.Context(), emptyBaseline(), primaryEvents, false, false)
	require.Equal(t, primaryEvents, copied)
	require.NotSame(t, &primaryEvents[0], &copied[0])
}
